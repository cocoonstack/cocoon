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

func (b *Bridge) Verify(_ context.Context, vmID string) error {
	if _, err := netlink.LinkByName(tapName(vmID, 0)); err != nil {
		return fmt.Errorf("tap %s: %w", tapName(vmID, 0), err)
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
		queues := network.ResolveQueues(spec.Queues, vmCfg.CPU)
		if cErr := network.CreateTAP(name, queues); cErr != nil {
			return nil, cErr
		}
		added = append(added, spec.Index)

		tap, lErr := netlink.LinkByName(name)
		if lErr != nil {
			return nil, fmt.Errorf("find tap %s: %w", name, lErr)
		}

		if mErr := netlink.LinkSetMaster(tap, br); mErr != nil {
			return nil, fmt.Errorf("add %s to %s: %w", name, b.bridgeDev, mErr)
		}

		_ = netlink.LinkSetLearning(tap, false)
		if mtu := br.Attrs().MTU; mtu > 0 {
			_ = netlink.LinkSetMTU(tap, mtu)
		}
		_ = network.TuneTAP(tap)

		if uErr := netlink.LinkSetUp(tap); uErr != nil {
			return nil, fmt.Errorf("set %s up: %w", name, uErr)
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
	gc.Register(orch, GCModule(b.conf.RootDir))
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
