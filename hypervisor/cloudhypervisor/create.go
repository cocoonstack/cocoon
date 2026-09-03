package cloudhypervisor

import (
	"context"
	"fmt"
	"os"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/metadata"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (ch *CloudHypervisor) Create(ctx context.Context, id string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, net types.NetSetup, bootCfg *types.BootConfig) (*types.VM, error) {
	return ch.CreateSequence(ctx, id, hypervisor.CreateSpec{
		VMCfg:          vmCfg,
		StorageConfigs: storageConfigs,
		Net:            net,
		BootConfig:     bootCfg,
		Prepare: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, sc []*types.StorageConfig, net types.NetSetup, boot *types.BootConfig) ([]*types.StorageConfig, error) {
			if boot != nil && boot.KernelPath != "" {
				return ch.prepareOCI(ctx, vmID, vmCfg, sc, net.NetworkConfigs, boot)
			}
			return ch.prepareCloudimg(ctx, vmID, vmCfg, sc, net.NetworkConfigs)
		},
	})
}

func (ch *CloudHypervisor) prepareOCI(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, boot *types.BootConfig) ([]*types.StorageConfig, error) {
	storageConfigs, err := hypervisor.PrepareOCICOW(ctx, ch.conf.COWRawPath(vmID), vmCfg.Storage, storageConfigs)
	if err != nil {
		return nil, err
	}
	dataDisks, err := hypervisor.PrepareDataDisks(ctx, ch.conf.VMRunDir(vmID), vmCfg.DataDisks)
	if err != nil {
		return nil, err
	}
	storageConfigs = append(storageConfigs, dataDisks...)
	dns, err := ch.conf.DNSServers()
	if err != nil {
		return nil, fmt.Errorf("parse DNS servers: %w", err)
	}
	boot.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	return storageConfigs, nil
}

func (ch *CloudHypervisor) prepareCloudimg(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig) ([]*types.StorageConfig, error) {
	if len(storageConfigs) == 0 {
		return nil, fmt.Errorf("cloudimg: no base image StorageConfig")
	}
	basePath := storageConfigs[0].Path
	overlayPath := ch.conf.OverlayPath(vmID)

	if err := utils.RunQemuImg(ctx, "create", "-f", "qcow2", "-F", "qcow2", "-b", basePath, overlayPath); err != nil {
		return nil, err
	}

	if vmCfg.Storage > 0 {
		if err := qemuExpandImage(ctx, overlayPath, vmCfg.Storage); err != nil {
			return nil, fmt.Errorf("expand overlay: %w", err)
		}
	}

	dataDisks, err := hypervisor.PrepareDataDisks(ctx, ch.conf.VMRunDir(vmID), vmCfg.DataDisks)
	if err != nil {
		return nil, err
	}

	configs := []*types.StorageConfig{
		{Path: overlayPath, RO: false, Role: types.StorageRoleCOW},
	}
	configs = append(configs, dataDisks...)

	if vmCfg.Windows {
		return configs, nil
	}

	if err := ch.generateCidata(vmID, vmCfg, networkConfigs, configs); err != nil {
		return nil, err
	}
	cidataPath := ch.conf.CidataPath(vmID)
	configs = append(configs, &types.StorageConfig{Path: cidataPath, RO: true, Role: types.StorageRoleCidata})
	return configs, nil
}

// generateCidata writes the NoCloud cidata image. storageConfigs lets cidata pick up Role==Data disks for auto-mount via /dev/disk/by-id/virtio-<serial>.
func (ch *CloudHypervisor) generateCidata(vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig, storageConfigs []*types.StorageConfig) error {
	dns, err := ch.conf.DNSServers()
	if err != nil {
		return fmt.Errorf("parse DNS servers: %w", err)
	}
	metaCfg := &metadata.Config{
		InstanceID: vmID,
		Hostname:   vmCfg.Name,
		Username:   vmCfg.User,
		Password:   vmCfg.Password,
		DNS:        dns,
		Mounts:     buildMountSpecs(storageConfigs),
	}
	for _, n := range networkConfigs {
		if n == nil || n.MAC == "" {
			continue
		}
		ni := metadata.NetworkInfo{MAC: n.MAC}
		if n.Network != nil {
			ni.IP = n.Network.IP
			ni.Prefix = n.Network.Prefix
			ni.Gateway = n.Network.Gateway
		}
		metaCfg.Networks = append(metaCfg.Networks, ni)
	}

	cidataPath := ch.conf.CidataPath(vmID)
	f, err := os.OpenFile(cidataPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create cidata: %w", err)
	}
	if err := metadata.Generate(f, metaCfg); err != nil {
		_ = f.Close()
		return fmt.Errorf("generate cidata: %w", err)
	}
	return f.Close()
}

// buildMountSpecs derives cloud-init mounts (defaults,nofail) for Role==Data disks with non-empty MountPoint and a known FSType.
func buildMountSpecs(configs []*types.StorageConfig) []metadata.MountSpec {
	var out []metadata.MountSpec
	for _, sc := range configs {
		if sc.Role != types.StorageRoleData || sc.MountPoint == "" || sc.FSType == "" || sc.FSType == types.FSTypeNone {
			continue
		}
		out = append(out, metadata.MountSpec{
			Device:     "/dev/disk/by-id/virtio-" + sc.Serial,
			MountPoint: sc.MountPoint,
			FSType:     sc.FSType,
			Options:    "defaults,nofail",
		})
	}
	return out
}
