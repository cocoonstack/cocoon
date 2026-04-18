package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/metadata"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// CowSerial is re-exported for backward compatibility with cmd/vm/debug.go.
const CowSerial = hypervisor.CowSerial

// Create reserves a VM record, prepares disks, and leaves the VM in Created state.
func (ch *CloudHypervisor) Create(ctx context.Context, id string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, bootCfg *types.BootConfig) (_ *types.VM, err error) {
	// Reject over-core requests before any on-disk work.
	if err = hypervisor.ValidateHostCPU(vmCfg.CPU); err != nil {
		return nil, err
	}

	now := time.Now()
	runDir := ch.conf.VMRunDir(id)
	logDir := ch.conf.VMLogDir(id)

	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	// Cleanup is idempotent, so defer it once.
	defer func() {
		if err != nil {
			_ = hypervisor.RemoveVMDirs(runDir, logDir)
			ch.RollbackCreate(ctx, id, vmCfg.Name)
		}
	}()

	if err = ch.ReserveVM(ctx, id, vmCfg, blobIDs, runDir, logDir); err != nil {
		return nil, fmt.Errorf("reserve VM record: %w", err)
	}

	if err = utils.EnsureDirs(runDir, logDir); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	var bootCopy *types.BootConfig
	if bootCfg != nil {
		b := *bootCfg
		bootCopy = &b
	}

	var preparedStorage []*types.StorageConfig
	if bootCopy != nil && bootCopy.KernelPath != "" {
		preparedStorage, err = ch.prepareOCI(ctx, id, vmCfg, storageConfigs, networkConfigs, bootCopy)
	} else {
		preparedStorage, err = ch.prepareCloudimg(ctx, id, vmCfg, storageConfigs, networkConfigs)
	}
	if err != nil {
		return nil, err
	}

	info := &types.VM{
		ID: id, Hypervisor: typ, State: types.VMStateCreated,
		Config:         *vmCfg,
		StorageConfigs: preparedStorage,
		NetworkConfigs: networkConfigs,
		CreatedAt:      now, UpdatedAt: now,
	}
	rec := hypervisor.VMRecord{
		VM:           *info,
		BootConfig:   bootCopy,
		ImageBlobIDs: blobIDs,
		RunDir:       runDir,
		LogDir:       logDir,
	}
	if err := ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		idx.VMs[id] = &rec
		return nil
	}); err != nil {
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	return info, nil
}

// prepareOCI creates the raw COW disk and final kernel cmdline.
func (ch *CloudHypervisor) prepareOCI(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, boot *types.BootConfig) ([]*types.StorageConfig, error) {
	cowPath := ch.conf.COWRawPath(vmID)

	// Create the sparse COW file before truncating it.
	f, err := os.OpenFile(cowPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("create COW: %w", err)
	}
	_ = f.Close()
	if err = os.Truncate(cowPath, vmCfg.Storage); err != nil {
		return nil, fmt.Errorf("truncate COW: %w", err)
	}
	// mkfs.ext4
	out, err := exec.CommandContext(ctx, //nolint:gosec
		"mkfs.ext4", "-F", "-m", "0", "-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,discard",
		cowPath,
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mkfs.ext4 COW: %s: %w", strings.TrimSpace(string(out)), err)
	}

	storageConfigs = append(storageConfigs, &types.StorageConfig{
		Path:   cowPath,
		RO:     false,
		Serial: CowSerial,
	})

	dns, err := ch.conf.DNSServers()
	if err != nil {
		return nil, fmt.Errorf("parse DNS servers: %w", err)
	}
	boot.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	return storageConfigs, nil
}

// prepareCloudimg creates the overlay and optional cidata disk.
func (ch *CloudHypervisor) prepareCloudimg(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig) ([]*types.StorageConfig, error) {
	if len(storageConfigs) == 0 {
		return nil, fmt.Errorf("cloudimg: no base image StorageConfig")
	}
	basePath := storageConfigs[0].Path
	overlayPath := ch.conf.OverlayPath(vmID)

	if out, err := exec.CommandContext(ctx, //nolint:gosec
		"qemu-img", "create", "-f", "qcow2", "-F", "qcow2",
		"-b", basePath, overlayPath,
	).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("qemu-img create overlay: %s: %w", strings.TrimSpace(string(out)), err)
	}

	if vmCfg.Storage > 0 {
		if err := qemuExpandImage(ctx, overlayPath, vmCfg.Storage, false); err != nil {
			return nil, fmt.Errorf("expand overlay: %w", err)
		}
	}

	if vmCfg.Windows {
		return []*types.StorageConfig{
			{Path: overlayPath, RO: false},
		}, nil
	}

	if err := ch.generateCidata(vmID, vmCfg, networkConfigs); err != nil {
		return nil, err
	}

	cidataPath := ch.conf.CidataPath(vmID)
	return []*types.StorageConfig{
		{Path: overlayPath, RO: false},
		{Path: cidataPath, RO: true},
	}, nil
}

// generateCidata writes the NoCloud cidata image used by Create and Clone.
func (ch *CloudHypervisor) generateCidata(vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig) error {
	dns, err := ch.conf.DNSServers()
	if err != nil {
		return fmt.Errorf("parse DNS servers: %w", err)
	}
	metaCfg := &metadata.Config{
		InstanceID:   vmID,
		Hostname:     vmCfg.Name,
		RootPassword: ch.conf.DefaultRootPassword,
		DNS:          dns,
	}
	for _, n := range networkConfigs {
		if n == nil || n.Mac == "" {
			continue
		}
		ni := metadata.NetworkInfo{Mac: n.Mac}
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
