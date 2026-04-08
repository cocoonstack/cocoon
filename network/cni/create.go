package cni

import (
	"context"
	"fmt"

	"github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const defaultQueueSize = 256

// Config creates the network namespace, runs CNI ADD for each NIC, sets up
// TC redirect (eth↔tap) inside the netns, and returns NetworkConfigs ready for CH --net.
//
// Flow per NIC:
//  1. Create named netns cocoon-{vmID}
//  2. CNI ADD (containerID=vmID, netns path, ifName=eth{i})
//  3. Inside netns: flush eth{i} IP, create a per-VM tap, wire via TC ingress mirred
//  4. Return NetworkConfig{Tap: stable per-VM tap, Mac: generated, Network: CNI result}
func (c *CNI) Config(ctx context.Context, vmID string, numNICs int, vmCfg *types.VMConfig, existing ...*types.NetworkConfig) (configs []*types.NetworkConfig, retErr error) {
	if c.cniConf == nil {
		return nil, fmt.Errorf("%w: no conflist found in %s", network.ErrNotConfigured, c.conf.CNIConfDir)
	}
	confList, err := c.confListByName(vmCfg.Network)
	if err != nil {
		return nil, err
	}
	// Record the resolved name so it's persisted in the VM record.
	// Ensures recovery uses the exact same conflist even if the default changes.
	// This intentionally mutates the caller's VMConfig (documented on the interface).
	vmCfg.Network = confList.Name
	logger := log.WithFunc("cni.Config")

	nsName := netnsName(vmID)
	nsPath := netnsPath(vmID)

	// Step 1: create named network namespace (platform-specific).
	if err := createNetns(nsName); err != nil {
		return nil, fmt.Errorf("create netns %s: %w", nsName, err)
	}

	// Track successfully added CNI interfaces for rollback.
	// If store.Update at the end fails, retErr != nil triggers this defer.
	// CNI DEL can run without persisted records (it uses RuntimeConf, not our DB).
	var addedIFs []string
	defer func() {
		if retErr == nil {
			return
		}
		// Rollback: CNI DEL for each successfully added NIC to release IPAM.
		for _, ifn := range addedIFs {
			rt := &libcni.RuntimeConf{
				ContainerID: vmID,
				NetNS:       nsPath,
				IfName:      ifn,
			}
			if delErr := c.cniConf.DelNetworkList(ctx, confList, rt); delErr != nil {
				logger.Warnf(ctx, "rollback CNI DEL %s/%s: %v", vmID, ifn, delErr)
			}
		}
		_ = deleteNetns(ctx, nsName)
	}()

	for i := range numNICs {
		ifName := fmt.Sprintf("eth%d", i)
		tapName := tapNameForVM(vmID, i)

		// Step 2: CNI ADD — creates veth pair, assigns IP via IPAM.
		rt := &libcni.RuntimeConf{
			ContainerID: vmID,
			NetNS:       nsPath,
			IfName:      ifName,
		}

		// Recovery: release stale IPAM allocation, then re-add requesting
		// the same IP. After host reboot, IPAM state files survive on disk
		// but the netns is gone. DEL clears the old record; the IP= CNI_ARG
		// tells host-local to allocate exactly the original address so the
		// guest's static IP config still matches.
		if i < len(existing) && existing[i] != nil {
			if delErr := c.cniConf.DelNetworkList(ctx, confList, rt); delErr != nil {
				logger.Warnf(ctx, "pre-recovery CNI DEL %s/%s: %v (continuing)", vmID, ifName, delErr)
			}
			if existing[i].Network != nil && existing[i].Network.IP != "" {
				rt.Args = [][2]string{{"IgnoreUnknown", "1"}, {"IP", existing[i].Network.IP}}
			}
		}

		cniResult, err := c.cniConf.AddNetworkList(ctx, confList, rt)
		if err != nil {
			return nil, fmt.Errorf("cni add %s/%s: %w", vmID, ifName, err)
		}
		addedIFs = append(addedIFs, ifName)

		netInfo, err := extractNetworkInfo(cniResult)
		if err != nil {
			return nil, fmt.Errorf("parse CNI result: %w", err)
		}

		// Step 3: inside netns — flush IP, create tap, wire via TC redirect (platform-specific).
		// Returns eth0's MAC so the guest virtio-net uses the same address,
		// required for anti-spoofing CNI plugins (Cilium, Calico eBPF, VPC ENI).
		// On recovery, overrideMAC restores the original veth MAC to match
		// the persisted CH --net mac= value.
		var overrideMAC string
		if i < len(existing) && existing[i] != nil {
			overrideMAC = existing[i].Mac
		}
		mac, setupErr := setupTCRedirect(nsPath, ifName, tapName, vmCfg.CPU, overrideMAC)
		if setupErr != nil {
			return nil, fmt.Errorf("setup tc-redirect %s: %w", vmID, setupErr)
		}

		configs = append(configs, &types.NetworkConfig{
			Tap:       tapName,
			Mac:       mac,
			NumQueues: netNumQueues(vmCfg.CPU),
			QueueSize: defaultQueueSize,
			NetnsPath: nsPath,
			Network:   netInfo,
		})

		var logIP, logGW string
		if netInfo != nil {
			logIP = netInfo.IP
			logGW = netInfo.Gateway
		}
		logger.Debugf(ctx, "NIC %d: %s ip=%s gw=%s tap=%s mac=%s",
			i, ifName, logIP, logGW, tapName, mac)
	}

	// Recovery: DB records survived reboot, nothing to write.
	if len(existing) > 0 {
		return configs, nil
	}

	// Step 4: persist network records to DB.
	return configs, c.store.Update(ctx, func(idx *networkIndex) error {
		for i, cfg := range configs {
			netID, genErr := utils.GenerateID()
			if genErr != nil {
				return genErr
			}
			var net types.Network
			if cfg.Network != nil {
				net = *cfg.Network
			}
			idx.Networks[netID] = &networkRecord{
				ID:      netID,
				Type:    confList.Name,
				Network: net,
				VMID:    vmID,
				IfName:  fmt.Sprintf("eth%d", i),
			}
		}
		return nil
	})
}

func tapNameForVM(vmID string, nic int) string {
	const vmIDPrefixLen = 8
	if len(vmID) > vmIDPrefixLen {
		vmID = vmID[:vmIDPrefixLen]
	}
	return fmt.Sprintf("tap%s-%d", vmID, nic)
}

// netNumQueues returns the virtio-net num_queues for a given CPU count.
// Each vCPU gets a TX/RX queue pair: cpu <= 1 → 2 (single pair), cpu > 1 → cpu * 2.
func netNumQueues(cpu int) int {
	if cpu <= 1 {
		return 2 //nolint:mnd
	}
	return cpu * 2 //nolint:mnd
}

// extractNetworkInfo parses the CNI ADD result into types.Network.
// Returns (nil, nil) when CNI returns no IPs (e.g. macvlan without IPAM),
// indicating the guest should use DHCP.
func extractNetworkInfo(result cnitypes.Result) (*types.Network, error) {
	newResult, err := current.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("convert CNI result: %w", err)
	}
	if len(newResult.IPs) == 0 {
		return nil, nil
	}

	// Find the first IPv4 address. Dual-stack CNI plugins may return IPv6 first.
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
