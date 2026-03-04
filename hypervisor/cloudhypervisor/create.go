package cloudhypervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/metadata"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// CowSerial is the well-known virtio serial for the COW disk attached to OCI VMs.
const CowSerial = "cocoon-cow"

// Create registers a new VM, prepares the COW disk, and persists the record.
// The VM is left in Created state — call Start to launch it.
//
// To avoid a race with GC (which scans directories and removes those not in
// the DB), we write a placeholder record first, then create directories and
// prepare disks, and finally update the record to Created state.
func (ch *CloudHypervisor) Create(ctx context.Context, id string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, bootCfg *types.BootConfig) (*types.VM, error) {
	var err error
	now := time.Now()
	runDir := ch.conf.VMRunDir(id)
	logDir := ch.conf.VMLogDir(id)

	blobIDs := extractBlobIDs(storageConfigs, bootCfg)

	// Rollback on any failure after the placeholder is written.
	// All cleanup ops are idempotent — safe even if dirs/records don't exist yet.
	success := false
	defer func() {
		if !success {
			_ = removeVMDirs(runDir, logDir)
			ch.rollbackCreate(ctx, id, vmCfg.Name)
		}
	}()

	// Step 1: write a placeholder record so GC won't treat our dirs as orphans.
	if err = ch.reserveVM(ctx, id, vmCfg, blobIDs, runDir, logDir); err != nil {
		return nil, fmt.Errorf("reserve VM record: %w", err)
	}

	// Step 2: create directories and prepare disks.
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

	// Step 3: finalize the record with full data and Created state.
	info := types.VM{
		ID: id, State: types.VMStateCreated,
		Config:         *vmCfg,
		StorageConfigs: preparedStorage,
		NetworkConfigs: networkConfigs,
		CreatedAt:      now, UpdatedAt: now,
	}
	rec := hypervisor.VMRecord{
		VM:           info,
		BootConfig:   bootCopy,
		ImageBlobIDs: blobIDs,
		RunDir:       runDir,
		LogDir:       logDir,
	}
	if err := ch.store.Update(ctx, func(idx *hypervisor.VMIndex) error {
		idx.VMs[id] = &rec
		return nil
	}); err != nil {
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}

	success = true
	return &info, nil
}

// prepareOCI creates a raw COW disk, appends the COW StorageConfig, and builds
// the kernel cmdline with layer/cow serial mappings.
// Returns the updated StorageConfig slice.
func (ch *CloudHypervisor) prepareOCI(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, boot *types.BootConfig) ([]*types.StorageConfig, error) {
	cowPath := ch.conf.COWRawPath(vmID)

	// Create sparse COW file
	// os.Truncate requires the file to exist; create it first.
	f, err := os.OpenFile(cowPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("create COW: %w", err)
	}
	_ = f.Close()
	if err := os.Truncate(cowPath, vmCfg.Storage); err != nil {
		return nil, fmt.Errorf("truncate COW: %w", err)
	}
	// mkfs.ext4
	if out, err := exec.CommandContext(ctx, //nolint:gosec
		"mkfs.ext4", "-F", "-m", "0", "-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,discard",
		cowPath,
	).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("mkfs.ext4 COW: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Append COW StorageConfig.
	storageConfigs = append(storageConfigs, &types.StorageConfig{
		Path:   cowPath,
		RO:     false,
		Serial: CowSerial,
	})

	boot.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, ch.conf.DNSServers())
	return storageConfigs, nil
}

// prepareCloudimg creates a qcow2 COW overlay backed by the base image blob.
// Returns the updated StorageConfig slice (replaced with the overlay).
func (ch *CloudHypervisor) prepareCloudimg(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig) ([]*types.StorageConfig, error) {
	if len(storageConfigs) == 0 {
		return nil, fmt.Errorf("cloudimg: no base image StorageConfig")
	}
	basePath := storageConfigs[0].Path
	overlayPath := ch.conf.OverlayPath(vmID)

	// qemu-img create -f qcow2 -F qcow2 -b <base> <overlay>
	if out, err := exec.CommandContext(ctx, //nolint:gosec
		"qemu-img", "create", "-f", "qcow2", "-F", "qcow2",
		"-b", basePath, overlayPath,
	).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("qemu-img create overlay: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// qemu-img resize <overlay> <size>
	if vmCfg.Storage > 0 {
		sizeBytes := fmt.Sprintf("%d", vmCfg.Storage)
		if out, err := exec.CommandContext(ctx, //nolint:gosec
			"qemu-img", "resize", overlayPath, sizeBytes,
		).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("qemu-img resize overlay: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	// Generate cloud-init cidata disk.
	if err := ch.generateCidata(vmID, vmCfg, networkConfigs); err != nil {
		return nil, err
	}

	// Replace StorageConfigs with the overlay + cidata (base is accessed via backing file chain).
	cidataPath := ch.conf.CidataPath(vmID)
	return []*types.StorageConfig{
		{Path: overlayPath, RO: false},
		{Path: cidataPath, RO: true},
	}, nil
}

// extractBlobIDs extracts digest hexes from the original image StorageConfigs
// and BootConfig paths. Must be called before prepare transforms them.
func extractBlobIDs(storageConfigs []*types.StorageConfig, boot *types.BootConfig) map[string]struct{} {
	ids := make(map[string]struct{})
	if boot != nil && boot.KernelPath != "" {
		// OCI: erofs layer blobs + boot dir hexes.
		for _, s := range storageConfigs {
			if s.RO {
				ids[blobHexFromPath(s.Path)] = struct{}{}
			}
		}
		// boot/{hex}/vmlinuz → hex
		ids[filepath.Base(filepath.Dir(boot.KernelPath))] = struct{}{}
		if boot.InitrdPath != "" {
			ids[filepath.Base(filepath.Dir(boot.InitrdPath))] = struct{}{}
		}
	} else if len(storageConfigs) > 0 {
		// Cloudimg: base qcow2 blob hex (before overlay replaces it).
		ids[blobHexFromPath(storageConfigs[0].Path)] = struct{}{}
	}
	return ids
}

// generateCidata creates a fresh cloud-init NoCloud cidata disk image (FAT12)
// at the VM's canonical cidata path. Contains instance-id, hostname,
// root password, network-config, and write_files for cloud-init initialization.
// Used by both Create (prepareCloudimg) and Clone.
func (ch *CloudHypervisor) generateCidata(vmID string, vmCfg *types.VMConfig, networkConfigs []*types.NetworkConfig) error {
	metaCfg := &metadata.Config{
		InstanceID:   vmID,
		Hostname:     vmCfg.Name,
		RootPassword: ch.conf.DefaultRootPassword,
		DNS:          ch.conf.DNSServers(),
	}
	for _, n := range networkConfigs {
		if n.Network != nil && n.Network.IP != "" {
			ni := metadata.NetworkInfo{
				IP:     n.Network.IP,
				Prefix: n.Network.Prefix,
				Mac:    n.Mac,
			}
			if n.Network.Gateway != "" {
				ni.Gateway = n.Network.Gateway
			}
			metaCfg.Networks = append(metaCfg.Networks, ni)
		}
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

// prefixToNetmask converts a CIDR prefix length to a dotted-decimal netmask string.
func prefixToNetmask(prefix int) string {
	mask := net.CIDRMask(prefix, 32)
	return net.IP(mask).String()
}

// dnsFromConfig returns the first two DNS servers for kernel ip= param.
func dnsFromConfig(servers []string) (string, string) {
	dns0, dns1 := "", ""
	if len(servers) > 0 {
		dns0 = servers[0]
	}
	if len(servers) > 1 {
		dns1 = servers[1]
	}
	return dns0, dns1
}
