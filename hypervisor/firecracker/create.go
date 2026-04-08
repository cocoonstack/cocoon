package firecracker

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Create registers a new VM, prepares the COW disk, and persists the record.
// The VM is left in Created state — call Start to launch it.
// FC only supports OCI images (direct kernel boot).
func (fc *Firecracker) Create(ctx context.Context, id string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, bootCfg *types.BootConfig) (_ *types.VM, err error) {
	now := time.Now()
	runDir := fc.conf.VMRunDir(id)
	logDir := fc.conf.VMLogDir(id)

	blobIDs := hypervisor.ExtractBlobIDs(storageConfigs, bootCfg)

	defer func() {
		if err != nil {
			_ = hypervisor.RemoveVMDirs(runDir, logDir)
			fc.RollbackCreate(ctx, id, vmCfg.Name)
		}
	}()

	if err = fc.ReserveVM(ctx, id, vmCfg, blobIDs, runDir, logDir); err != nil {
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

	preparedStorage, err := fc.prepareOCI(ctx, id, vmCfg, storageConfigs, networkConfigs, bootCopy)
	if err != nil {
		return nil, err
	}

	info := types.VM{
		ID: id, Hypervisor: typ, State: types.VMStateCreated,
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
	if err := fc.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		idx.VMs[id] = &rec
		return nil
	}); err != nil {
		return nil, fmt.Errorf("finalize VM record: %w", err)
	}
	return &info, nil
}

// prepareOCI creates a raw COW disk, appends the COW StorageConfig, and builds
// the kernel cmdline with device-path mappings (FC lacks virtio serial support).
func (fc *Firecracker) prepareOCI(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, boot *types.BootConfig) ([]*types.StorageConfig, error) {
	cowPath := fc.conf.COWRawPath(vmID)

	f, err := os.OpenFile(cowPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("create COW: %w", err)
	}
	_ = f.Close()
	if err = os.Truncate(cowPath, vmCfg.Storage); err != nil {
		return nil, fmt.Errorf("truncate COW: %w", err)
	}
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
		Serial: hypervisor.CowSerial,
	})

	// FC requires uncompressed ELF kernel (vmlinux), not compressed vmlinuz.
	// Extract and cache vmlinux alongside vmlinuz if needed.
	if boot != nil && boot.KernelPath != "" {
		vmlinuxPath, extractErr := EnsureVmlinux(boot.KernelPath)
		if extractErr != nil {
			return nil, fmt.Errorf("extract vmlinux: %w", extractErr)
		}
		boot.KernelPath = vmlinuxPath
	}

	dns, err := fc.conf.DNSServers()
	if err != nil {
		return nil, fmt.Errorf("parse DNS servers: %w", err)
	}
	if boot != nil {
		boot.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmCfg.Name, dns)
	}
	return storageConfigs, nil
}

// EnsureVmlinux returns the path to an uncompressed ELF kernel.
// If the kernel at path is already ELF, returns path as-is.
// Otherwise, extracts the uncompressed kernel from the compressed vmlinuz
// and caches it as "vmlinux" in the same directory.
func EnsureVmlinux(kernelPath string) (string, error) {
	elfMagic := []byte{0x7f, 'E', 'L', 'F'}

	// Quick check: read just the magic bytes to detect ELF without loading the full file.
	f, err := os.Open(kernelPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("open kernel: %w", err)
	}
	var magic [4]byte
	_, err = io.ReadFull(f, magic[:])
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("read kernel magic: %w", err)
	}
	if bytes.Equal(magic[:], elfMagic) {
		return kernelPath, nil // already uncompressed
	}

	// Check cache before doing expensive decompression.
	vmlinuxPath := filepath.Join(filepath.Dir(kernelPath), "vmlinux")
	if _, statErr := os.Stat(vmlinuxPath); statErr == nil {
		return vmlinuxPath, nil // already cached
	}

	// Full read only when decompression is needed.
	data, err := os.ReadFile(kernelPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read kernel: %w", err)
	}

	// Try known compression formats: zstd (Ubuntu 24.04+), then gzip.
	decompressed, decompErr := decompressKernel(data)
	if decompErr != nil {
		return "", fmt.Errorf("decompress kernel from %s: %w (FC requires uncompressed ELF kernel)", kernelPath, decompErr)
	}

	// Write atomically via temp file + rename to prevent concurrent readers
	// from observing a partially written kernel.
	tmpFile, tmpErr := os.CreateTemp(filepath.Dir(vmlinuxPath), ".vmlinux-*")
	if tmpErr != nil {
		return "", fmt.Errorf("create temp vmlinux: %w", tmpErr)
	}
	tmpPath := tmpFile.Name()
	if _, writeErr := tmpFile.Write(decompressed); writeErr != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write vmlinux: %w", writeErr)
	}
	if closeErr := tmpFile.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close vmlinux: %w", closeErr)
	}
	if renameErr := os.Rename(tmpPath, vmlinuxPath); renameErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename vmlinux: %w", renameErr)
	}
	return vmlinuxPath, nil
}

// decompressKernel scans a bzImage for known compression formats and
// returns the decompressed ELF kernel.
func decompressKernel(data []byte) ([]byte, error) {
	type kernelCodec struct {
		name  string
		magic []byte
	}
	formats := []kernelCodec{
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
		{"gzip", []byte{0x1f, 0x8b, 0x08}},
	}

	elfMagic := []byte{0x7f, 'E', 'L', 'F'}
	for _, f := range formats {
		offset := bytes.Index(data, f.magic)
		if offset < 0 {
			continue
		}
		payload := data[offset:]
		var decompressed []byte
		var err error
		switch f.name {
		case "zstd":
			decompressed, err = decompressZstd(payload)
		case "gzip":
			decompressed, err = decompressGzip(payload)
		}
		if err != nil || !bytes.HasPrefix(decompressed, elfMagic) {
			continue
		}
		return decompressed, nil
	}
	return nil, fmt.Errorf("no supported compression format found (tried zstd, gzip)")
}

func decompressZstd(data []byte) ([]byte, error) {
	// Use zstd CLI: the Go library's decoder may not handle the kernel's
	// zstd stream correctly, and the bzImage has trailing data after the
	// zstd frame which causes errors even with valid decompression.
	cmd := exec.Command("zstd", "-d", "-c", "--no-check") //nolint:gosec
	cmd.Stdin = bytes.NewReader(data)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run() // ignore exit code — trailing data after frame causes non-zero exit
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("zstd produced no output (is zstd installed?)")
	}
	return stdout.Bytes(), nil
}

func decompressGzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck
	return io.ReadAll(r)
}

// buildCmdline generates the kernel cmdline for FC VMs.
// FC doesn't support virtio serial, so disks are referenced by device path
// (/dev/vda, /dev/vdb, ...) based on the order drives are attached.
func buildCmdline(storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	nLayers := 0
	for _, s := range storageConfigs {
		if s.RO {
			nLayers++
		}
	}

	// Build layer device paths reversed (top layer first for overlayfs lowerdir)
	layerDevs := make([]string, nLayers)
	for i := range nLayers {
		layerDevs[nLayers-1-i] = devPath(i)
	}
	cowDev := devPath(nLayers)

	var cmdline strings.Builder
	// FC serial console is ttyS0 (not hvc0 like CH's virtio-console).
	// reboot=k: FC has no ACPI PM — use i8042 keyboard controller reset so
	// guest reboot/shutdown triggers FC process exit instead of hanging.
	fmt.Fprintf(&cmdline,
		"console=ttyS0 reboot=k loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
		strings.Join(layerDevs, ","), cowDev,
	)

	if len(networkConfigs) > 0 {
		cmdline.WriteString(" net.ifnames=0")
		cmdline.WriteString(hypervisor.BuildIPParams(networkConfigs, vmName, dnsServers))
	}

	return cmdline.String()
}

// devPath returns the virtio block device path for the i-th drive.
// Follows Linux naming: vda..vdz, vdaa..vdaz, vdba..vdbz, ...
func devPath(idx int) string {
	const letters = 26
	if idx < letters {
		return fmt.Sprintf("/dev/vd%c", 'a'+idx)
	}
	return fmt.Sprintf("/dev/vd%c%c", 'a'+(idx/letters)-1, 'a'+idx%letters)
}
