//go:build linux

package bridge

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

const typ = "bridge"

var _ network.Network = (*Bridge)(nil)

// Bridge is TAP-on-bridge; requires a pre-existing bridge with DHCP + routing.
type Bridge struct {
	conf      *config.Config
	bridgeDev string
	bridgeIdx int
}

// New: the bridge device must already exist.
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

func (b *Bridge) Type() string { return typ }

func (b *Bridge) Verify(_ context.Context, vmID string, expected []*types.NetworkConfig) error {
	// Legacy records persisted no NetworkConfigs, so empty means "assume tap0"
	// — callers that legitimately resized to zero NICs must not call Verify.
	if len(expected) == 0 {
		if _, err := netlink.LinkByName(tapName(vmID, 0)); err != nil {
			return fmt.Errorf("tap %s: %w", tapName(vmID, 0), err)
		}
		return nil
	}
	for _, nc := range expected {
		if nc == nil || nc.TAP == "" {
			continue
		}
		if _, err := netlink.LinkByName(nc.TAP); err != nil {
			return fmt.Errorf("tap %s: %w", nc.TAP, err)
		}
	}
	return nil
}

// Prepare is a no-op (bridge has no netns).
func (b *Bridge) Prepare(_ context.Context, _ string, _ *types.VMConfig) (string, error) {
	return "", nil
}

func (b *Bridge) Add(ctx context.Context, vmID string, vmCfg *types.VMConfig, specs ...network.AddSpec) (configs []*types.NetworkConfig, retErr error) {
	if len(specs) == 0 {
		return nil, nil
	}
	logger := log.WithFunc("bridge.Add")

	br, err := netlink.LinkByIndex(b.bridgeIdx)
	if err != nil {
		return nil, fmt.Errorf("find bridge: %w", err)
	}

	added := make([]int, 0, len(specs))
	defer func() {
		if retErr == nil || len(added) == 0 {
			return
		}
		_ = tearDownTAPs(vmID, added, true)
	}()

	configs = make([]*types.NetworkConfig, 0, len(specs))
	for _, spec := range specs {
		name := tapName(vmID, spec.Index)
		mac := generateMAC()
		if spec.Existing != nil {
			mac = spec.Existing.MAC
		}
		// Fresh adds only: a same-name TAP is an interrupted-resize leftover that would wedge every retry; recovery specs keep the EEXIST failure (their slot may hold a live VMM's TAP).
		if spec.Existing == nil {
			if old, lErr := netlink.LinkByName(name); lErr == nil {
				if delErr := netlink.LinkDel(old); delErr != nil {
					return nil, fmt.Errorf("reclaim stale tap %s: %w", name, delErr)
				}
			}
		}
		queues := network.ResolveQueues(spec.Queues, vmCfg.CPU)
		tapIndex, cErr := network.CreateTAP(name, queues)
		if cErr != nil {
			return nil, cErr
		}
		added = append(added, spec.Index)

		if aErr := attachBridgeUp(tapIndex, b.bridgeIdx, br.Attrs().MTU); aErr != nil {
			return nil, aErr
		}

		configs = append(configs, &types.NetworkConfig{
			TAP:       name,
			MAC:       mac,
			NumQueues: queues,
			QueueSize: network.ResolveQueueSize(vmCfg.QueueSize),
			Backend:   types.BackendBridge,
			BridgeDev: b.bridgeDev,
		})
		logger.Debugf(ctx, "NIC %d: tap=%s mac=%s bridge=%s", spec.Index, name, mac, b.bridgeDev)
	}
	return configs, nil
}

func (b *Bridge) Remove(_ context.Context, vmID string, indices ...int) error {
	return tearDownTAPs(vmID, indices, false)
}

// Quiesce and Unquiesce are no-ops: bridge TAPs sit directly on the host bridge, with no TC redirect to storm when the VM stops.
func (b *Bridge) Quiesce(_ context.Context, _ string) error   { return nil }
func (b *Bridge) Unquiesce(_ context.Context, _ string) error { return nil }

func (b *Bridge) Delete(_ context.Context, vmIDs []string) ([]string, error) {
	return CleanupTAPs(vmIDs), nil
}

// Inspect: bridge has no persistent records.
func (b *Bridge) Inspect(_ context.Context, _ string) (*types.Network, error) {
	return nil, nil
}

// List: bridge has no persistent records.
func (b *Bridge) List(_ context.Context) ([]*types.Network, error) {
	return nil, nil
}

// RegisterGC reclaims orphan bt* TAP devices.
func (b *Bridge) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, GCModule())
}

// CleanupTAPs removes bridge TAP devices per VM ID; safe without a Bridge instance.
func CleanupTAPs(vmIDs []string) []string {
	cleaned := make([]string, 0, len(vmIDs))
	for _, vmID := range vmIDs {
		var indices []int
		for i := 0; ; i++ {
			if _, err := netlink.LinkByName(tapName(vmID, i)); err != nil {
				break
			}
			indices = append(indices, i)
		}
		_ = tearDownTAPs(vmID, indices, true)
		cleaned = append(cleaned, vmID)
	}
	return cleaned
}

// attachBridgeUp enslaves a TAP to the bridge, applies MTU/txqlen/GRO tuning and brings it up in one RTM_SETLINK, paying the node-wide rtnl lock once.
func attachBridgeUp(tapIndex, bridgeIndex, mtu int) error {
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(tapIndex) //nolint:gosec // kernel-issued ifindex fits int32
	msg.Flags = unix.IFF_UP
	msg.Change = unix.IFF_UP
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFLA_MASTER, nl.Uint32Attr(uint32(bridgeIndex)))) //nolint:gosec // kernel-issued ifindex fits uint32
	req.AddData(nl.NewRtAttr(unix.IFLA_TXQLEN, nl.Uint32Attr(network.TAPTxQueueLen)))
	req.AddData(nl.NewRtAttr(unix.IFLA_GRO_MAX_SIZE, nl.Uint32Attr(network.GROMaxSize)))
	if mtu > 0 {
		req.AddData(nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(mtu)))) //nolint:gosec // guarded > 0; kernel MTU fits uint32
	}
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("attach tap %d to bridge %d: %w", tapIndex, bridgeIndex, err)
	}
	return nil
}

func tearDownTAPs(vmID string, indices []int, bestEffort bool) error {
	for _, i := range indices {
		name := tapName(vmID, i)
		link, err := netlink.LinkByName(name)
		if err != nil {
			if bestEffort {
				continue
			}
			return fmt.Errorf("find tap %s: %w", name, err)
		}
		if err := netlink.LinkDel(link); err != nil {
			if bestEffort {
				continue
			}
			return fmt.Errorf("delete tap %s: %w", name, err)
		}
	}
	return nil
}

func tapName(vmID string, nic int) string {
	return network.TAPName(tapPrefix, vmID, nic)
}

func generateMAC() string {
	buf := make([]byte, 6) //nolint:mnd
	_, _ = rand.Read(buf)
	buf[0] = (buf[0] | 0x02) & 0xfe
	return net.HardwareAddr(buf).String()
}
