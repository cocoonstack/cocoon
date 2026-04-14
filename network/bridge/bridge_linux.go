//go:build linux

package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netlink"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// compile-time interface check.
var _ network.Network = (*Bridge)(nil)

const (
	typ              = "bridge"
	defaultQueueSize = 256
)

// Bridge implements network.Network by creating TAP devices and adding
// them directly to an existing Linux bridge. An external DHCP server
// on the bridge (e.g. dnsmasq) serves VM IPs. No veth, no TC, no
// netns — just TAP-on-bridge, the simplest possible VM networking.
//
// This backend is designed to work with cocoon-net's cni0 bridge or
// any pre-existing bridge that has DHCP + routing already set up.
type Bridge struct {
	conf      *config.Config
	bridgeDev string
	bridgeIdx int
}

// New creates a Bridge network provider. The bridge device must exist.
func New(conf *config.Config, bridgeDev string) (*Bridge, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if bridgeDev == "" {
		return nil, fmt.Errorf("bridge device name is required")
	}
	br, err := netlink.LinkByName(bridgeDev)
	if err != nil {
		return nil, fmt.Errorf("bridge %s: %w", bridgeDev, err)
	}
	if br.Type() != "bridge" {
		return nil, fmt.Errorf("%s is not a bridge (type: %s)", bridgeDev, br.Type())
	}
	return &Bridge{
		conf:      conf,
		bridgeDev: bridgeDev,
		bridgeIdx: br.Attrs().Index,
	}, nil
}

// Type returns the provider identifier.
func (b *Bridge) Type() string { return typ }

// Verify checks whether the TAP for a VM exists.
func (b *Bridge) Verify(_ context.Context, vmID string) error {
	if _, err := netlink.LinkByName(tapName(vmID, 0)); err != nil {
		return fmt.Errorf("tap %s: %w", tapName(vmID, 0), err)
	}
	return nil
}

// Config creates TAP devices and adds them to the bridge.
func (b *Bridge) Config(ctx context.Context, vmID string, numNICs int, vmCfg *types.VMConfig, existing ...*types.NetworkConfig) ([]*types.NetworkConfig, error) {
	logger := log.WithFunc("bridge.Config")

	br, err := netlink.LinkByIndex(b.bridgeIdx)
	if err != nil {
		return nil, fmt.Errorf("find bridge: %w", err)
	}

	var configs []*types.NetworkConfig
	for i := range numNICs {
		name := tapName(vmID, i)

		var mac string
		if i < len(existing) && existing[i] != nil {
			mac = existing[i].Mac
		} else {
			mac = generateMAC()
		}

		queues := network.NetNumQueues(vmCfg.CPU)
		if err := createTAP(name, queues); err != nil {
			return nil, fmt.Errorf("create tap %s: %w", name, err)
		}

		tap, err := netlink.LinkByName(name)
		if err != nil {
			return nil, fmt.Errorf("find tap %s: %w", name, err)
		}

		// Add TAP to bridge — this is the only wiring needed.
		if err := netlink.LinkSetMaster(tap, br); err != nil {
			_ = netlink.LinkDel(tap)
			return nil, fmt.Errorf("add %s to %s: %w", name, b.bridgeDev, err)
		}

		// Sync MTU from bridge.
		if mtu := br.Attrs().MTU; mtu > 0 {
			_ = netlink.LinkSetMTU(tap, mtu)
		}

		if err := netlink.LinkSetUp(tap); err != nil {
			_ = netlink.LinkDel(tap)
			return nil, fmt.Errorf("set %s up: %w", name, err)
		}

		configs = append(configs, &types.NetworkConfig{
			Tap:       name,
			Mac:       mac,
			NumQueues: queues,
			QueueSize: defaultQueueSize,
			Backend:   typ,
			BridgeDev: b.bridgeDev,
			// NetnsPath: empty — TAP is in host netns.
			// Network:   nil — IP comes from DHCP on the bridge.
		})
		logger.Debugf(ctx, "NIC %d: tap=%s mac=%s bridge=%s", i, name, mac, b.bridgeDev)
	}
	return configs, nil
}

// Delete removes TAP devices for the given VMs.
func (b *Bridge) Delete(_ context.Context, vmIDs []string) ([]string, error) {
	return CleanupTAPs(vmIDs), nil
}

// Inspect is not supported — bridge mode has no persistent records.
func (b *Bridge) Inspect(_ context.Context, _ string) (*types.Network, error) {
	return nil, nil
}

// List is not supported — bridge mode has no persistent records.
func (b *Bridge) List(_ context.Context) ([]*types.Network, error) {
	return nil, nil
}

// RegisterGC registers the bridge GC module that reclaims orphan bt* TAP devices.
func (b *Bridge) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, GCModule(b.conf.RootDir))
}

// CleanupTAPs removes bridge TAP devices for the given VM IDs.
// It does not require a Bridge instance and is safe to call
// even when no bridge TAPs exist (no-op per VM).
func CleanupTAPs(vmIDs []string) []string {
	var cleaned []string
	for _, vmID := range vmIDs {
		for i := range 8 { // max 8 NICs per VM
			name := tapName(vmID, i)
			l, err := netlink.LinkByName(name)
			if err != nil {
				break // no more TAPs for this VM
			}
			_ = netlink.LinkDel(l)
		}
		cleaned = append(cleaned, vmID)
	}
	return cleaned
}

func createTAP(name string, numQueues int) error {
	// CH uses queue pairs (TX+RX): queue_pairs = num_queues / 2.
	// Multi-queue requires queue_pairs > 1, i.e. num_queues > 2.
	// The TAP's IFF_MULTI_QUEUE flag must match CH's expectation,
	// otherwise CH's sysfs pre-flight check rejects the device.
	queuePairs := max(1, numQueues/2) //nolint:mnd
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs <= 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Queues:    queuePairs,
		Flags:     flags,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return err
	}
	for _, fd := range tap.Fds {
		_ = fd.Close()
	}
	return nil
}

func tapName(vmID string, nic int) string {
	return fmt.Sprintf("%s%s-%d", tapPrefix, network.VMIDPrefix(vmID), nic)
}

func generateMAC() string {
	buf := make([]byte, 6) //nolint:mnd
	_, _ = rand.Read(buf)
	buf[0] = (buf[0] | 0x02) & 0xfe
	return net.HardwareAddr(buf).String()
}
