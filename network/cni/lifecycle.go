package cni

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Prepare creates the per-VM netns; returns "" with no conflist.
func (c *CNI) Prepare(_ context.Context, vmID string, _ *types.VMConfig) (string, error) {
	if c.cniConf == nil {
		return "", nil
	}
	nsName := netnsName(vmID)
	nsPath := netnsPath(vmID)
	if _, err := ensureNetns(nsName, nsPath); err != nil {
		return "", fmt.Errorf("ensure netns %s: %w", nsName, err)
	}
	return nsPath, nil
}

// Add creates the netns (if absent) and allocates each NIC's CNI plumbing.
func (c *CNI) Add(ctx context.Context, vmID string, vmCfg *types.VMConfig, specs ...network.AddSpec) (configs []*types.NetworkConfig, retErr error) {
	if c.cniConf == nil {
		return nil, c.errNoConflist()
	}
	if len(specs) == 0 {
		return nil, nil
	}
	confList, err := c.confListByName(vmCfg.Network)
	if err != nil {
		return nil, err
	}
	vmCfg.Network = confList.Name
	logger := log.WithFunc("cni.Add")

	nsName := netnsName(vmID)
	nsPath := netnsPath(vmID)

	createdNetns, err := ensureNetns(nsName, nsPath)
	if err != nil {
		return nil, fmt.Errorf("ensure netns %s: %w", nsName, err)
	}

	addedIdx := make([]int, 0, len(specs))
	defer func() {
		if retErr == nil {
			return
		}
		for _, i := range addedIdx {
			ifn := fmt.Sprintf("eth%d", i)
			if delErr := c.cniDel(ctx, confList, vmID, nsPath, ifn); delErr != nil {
				logger.Warnf(ctx, "rollback CNI DEL %s/%s: %v", vmID, ifn, delErr)
			}
			// setupTCRedirect creates the TAP; it would leak if the netns persists.
			if !createdNetns {
				if delErr := deleteTAPInNetns(nsPath, tapNameForVM(vmID, i)); delErr != nil {
					logger.Warnf(ctx, "rollback tap delete %s: %v", tapNameForVM(vmID, i), delErr)
				}
			}
		}
		if createdNetns {
			_ = deleteNetns(ctx, nsName)
		}
	}()

	type freshNIC struct {
		index int
		cfg   *types.NetworkConfig
	}
	configs = make([]*types.NetworkConfig, 0, len(specs))
	fresh := make([]freshNIC, 0, len(specs))
	for _, spec := range specs {
		ifName := fmt.Sprintf("eth%d", spec.Index)
		tapName := tapNameForVM(vmID, spec.Index)

		rt := &libcni.RuntimeConf{ContainerID: vmID, NetNS: nsPath, IfName: ifName}
		if spec.Existing != nil {
			if delErr := c.cniDel(ctx, confList, vmID, nsPath, ifName); delErr != nil {
				logger.Warnf(ctx, "pre-recovery CNI DEL %s/%s: %v (continuing)", vmID, ifName, delErr)
			}
			if spec.Existing.Network != nil && spec.Existing.Network.IP != "" {
				rt.Args = [][2]string{{"IgnoreUnknown", "1"}, {"IP", spec.Existing.Network.IP}}
			}
		}

		cniResult, addErr := c.cniConf.AddNetworkList(ctx, confList, rt)
		if addErr != nil {
			return nil, fmt.Errorf("cni add %s/%s: %w", vmID, ifName, addErr)
		}
		addedIdx = append(addedIdx, spec.Index)

		netInfo, parseErr := extractNetworkInfo(cniResult)
		if parseErr != nil {
			return nil, fmt.Errorf("parse CNI result: %w", parseErr)
		}

		var overrideMAC string
		if spec.Existing != nil {
			overrideMAC = spec.Existing.MAC
		}
		queues := network.ResolveQueues(spec.Queues, vmCfg.CPU)
		mac, setupErr := setupTCRedirect(nsPath, ifName, tapName, queues, overrideMAC)
		if setupErr != nil {
			return nil, fmt.Errorf("setup tc-redirect %s: %w", vmID, setupErr)
		}

		cfg := &types.NetworkConfig{
			TAP:       tapName,
			MAC:       mac,
			NumQueues: queues,
			QueueSize: network.ResolveQueueSize(vmCfg.QueueSize),
			Backend:   types.BackendCNI,
			NetnsPath: nsPath,
			Network:   netInfo,
		}
		configs = append(configs, cfg)
		if spec.Existing == nil {
			fresh = append(fresh, freshNIC{index: spec.Index, cfg: cfg})
		}

		var logIP, logGW string
		if netInfo != nil {
			logIP = netInfo.IP
			logGW = netInfo.Gateway
		}
		logger.Debugf(ctx, "NIC %d: %s ip=%s gw=%s tap=%s mac=%s",
			spec.Index, ifName, logIP, logGW, tapName, mac)
	}

	return configs, c.store.Update(ctx, func(idx *networkIndex) error {
		for _, f := range fresh {
			netID := utils.GenerateID()
			var net types.Network
			if f.cfg.Network != nil {
				net = *f.cfg.Network
			}
			idx.Networks[netID] = &networkRecord{
				ID:      netID,
				Type:    confList.Name,
				Network: net,
				VMID:    vmID,
				IfName:  fmt.Sprintf("eth%d", f.index),
			}
		}
		return nil
	})
}

// Remove tears down NIC plumbing for the given indices; preserves the netns. A failed NIC keeps its DB record so retry / vm rm / GC can still release its CNI resources.
func (c *CNI) Remove(ctx context.Context, vmID string, indices ...int) error {
	if len(indices) == 0 {
		return nil
	}
	var records []networkRecord
	if err := c.store.With(ctx, func(idx *networkIndex) error {
		records = idx.byVMID(vmID)
		return nil
	}); err != nil {
		return fmt.Errorf("read network index: %w", err)
	}
	byIfName := make(map[string]networkRecord, len(records))
	for _, r := range records {
		byIfName[r.IfName] = r
	}
	picked := make([]networkRecord, 0, len(indices))
	for _, i := range indices {
		ifName := fmt.Sprintf("eth%d", i)
		rec, ok := byIfName[ifName]
		if !ok {
			return fmt.Errorf("nic %d (%s): no record", i, ifName)
		}
		picked = append(picked, rec)
	}
	downIDs, err := c.tearDownNICs(ctx, vmID, netnsPath(vmID), picked, true)
	return errors.Join(err, c.deleteRecords(ctx, downIDs))
}

func (c *CNI) cniDel(ctx context.Context, confList *libcni.NetworkConfigList, vmID, nsPath, ifName string) error {
	rt := &libcni.RuntimeConf{ContainerID: vmID, NetNS: nsPath, IfName: ifName}
	return c.cniConf.DelNetworkList(ctx, confList, rt)
}

func tapNameForVM(vmID string, nic int) string {
	return fmt.Sprintf("tap%s-%d", network.VMIDPrefix(vmID), nic)
}

// ensureNetns creates the netns if missing; bool reports whether this call did the creation.
func ensureNetns(name, nsPath string) (bool, error) {
	if _, err := os.Stat(nsPath); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := createNetns(name); err != nil {
		return false, err
	}
	return true, nil
}

// extractNetworkInfo converts a CNI ADD result into types.Network.
func extractNetworkInfo(result cnitypes.Result) (*types.Network, error) {
	newResult, err := current.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("convert CNI result: %w", err)
	}
	if len(newResult.IPs) == 0 {
		return nil, nil
	}

	for _, ipCfg := range newResult.IPs {
		if ipCfg.Address.IP.To4() != nil {
			ones, _ := ipCfg.Address.Mask.Size()
			info := &types.Network{
				IP:     ipCfg.Address.IP.String(),
				Prefix: ones,
			}
			if ipCfg.Gateway != nil {
				info.Gateway = ipCfg.Gateway.String()
			}
			return info, nil
		}
	}
	return nil, nil
}
