package firecracker

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

func (fc *Firecracker) Create(ctx context.Context, id string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, net types.NetSetup, bootCfg *types.BootConfig) (*types.VM, error) {
	// Backend-owned capability limits; cmd/ repeats them only as flag-specific fast-fails.
	if vmCfg.Windows {
		return nil, fmt.Errorf("firecracker does not support Windows guests")
	}
	if vmCfg.SharedMemory {
		return nil, fmt.Errorf("firecracker does not support shared memory (vhost-user-fs)")
	}
	if vmCfg.HugePages {
		return nil, fmt.Errorf("firecracker does not support hugepages (restore cannot map hugetlbfs-backed snapshots)")
	}
	if vmCfg.Mergeable {
		return nil, fmt.Errorf("firecracker does not support mergeable memory (no KSM madvise knob)")
	}
	if !hypervisor.IsDirectBoot(bootCfg) {
		return nil, fmt.Errorf("firecracker requires direct kernel boot (OCI image)")
	}
	return fc.CreateSequence(ctx, id, hypervisor.CreateSpec{
		VMCfg:          vmCfg,
		StorageConfigs: storageConfigs,
		Net:            net,
		BootConfig:     bootCfg,
		Prepare: func(ctx context.Context, vmID string, vmCfg *types.VMConfig, sc []*types.StorageConfig, net types.NetSetup, boot *types.BootConfig) ([]*types.StorageConfig, error) {
			return fc.prepareOCI(ctx, vmID, vmCfg, sc, net.NetworkConfigs, boot)
		},
	})
}

func (fc *Firecracker) prepareOCI(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, boot *types.BootConfig) ([]*types.StorageConfig, error) {
	storageConfigs, err := hypervisor.PrepareOCICOW(ctx, fc.conf.COWRawPath(vmID), vmCfg.Storage, storageConfigs)
	if err != nil {
		return nil, err
	}
	dataDisks, err := hypervisor.PrepareDataDisks(ctx, fc.conf.VMRunDir(vmID), vmCfg.DataDisks)
	if err != nil {
		return nil, err
	}
	storageConfigs = append(storageConfigs, dataDisks...)
	if err := EnsureVmlinuxBoot(boot); err != nil {
		return nil, err
	}
	if err := fc.setBootCmdline(boot, storageConfigs, networkConfigs, vmCfg.Name); err != nil {
		return nil, err
	}
	return storageConfigs, nil
}

// setBootCmdline rebuilds boot.Cmdline from the final disk/NIC layout; nil boot is a no-op.
func (fc *Firecracker) setBootCmdline(boot *types.BootConfig, storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, vmName string) error {
	if boot == nil {
		return nil
	}
	dns, err := fc.conf.DNSServers()
	if err != nil {
		return fmt.Errorf("parse DNS servers: %w", err)
	}
	boot.Cmdline = buildCmdline(storageConfigs, networkConfigs, vmName, dns)
	return nil
}

// DevPath maps idx to vda..vdz, vdaa..vdaz, vdba..vdbz, ...
func DevPath(idx int) string {
	const letters = 26
	if idx < letters {
		return fmt.Sprintf("/dev/vd%c", 'a'+idx)
	}
	return fmt.Sprintf("/dev/vd%c%c", 'a'+(idx/letters)-1, 'a'+idx%letters)
}

// EnsureVmlinuxBoot swaps boot.KernelPath for the uncompressed ELF FC requires; no-op without a kernel path.
func EnsureVmlinuxBoot(boot *types.BootConfig) error {
	if boot == nil || boot.KernelPath == "" {
		return nil
	}
	vmlinuxPath, err := EnsureVmlinux(boot.KernelPath)
	if err != nil {
		return fmt.Errorf("extract vmlinux: %w", err)
	}
	boot.KernelPath = vmlinuxPath
	return nil
}

// EnsureVmlinux decompresses kernelPath if needed and returns the ELF path.
func EnsureVmlinux(kernelPath string) (string, error) {
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
		return kernelPath, nil
	}

	vmlinuxPath := filepath.Join(filepath.Dir(kernelPath), "vmlinux")
	if utils.FileExists(vmlinuxPath) {
		return vmlinuxPath, nil
	}

	data, err := os.ReadFile(kernelPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read kernel: %w", err)
	}

	decompressed, decompErr := decompressKernel(data)
	if decompErr != nil {
		return "", fmt.Errorf("decompress kernel from %s: %w (FC requires uncompressed ELF kernel)", kernelPath, decompErr)
	}

	if err := utils.AtomicWriteFile(vmlinuxPath, decompressed, 0o600); err != nil {
		return "", fmt.Errorf("write vmlinux: %w", err)
	}
	return vmlinuxPath, nil
}

func decompressKernel(data []byte) ([]byte, error) {
	type kernelCodec struct {
		name   string
		magic  []byte
		decode func([]byte) ([]byte, error)
	}
	formats := []kernelCodec{
		{"zstd", utils.ZstdMagic, decompressZstd},
		// 3-byte gzip magic (deflate method byte included): bytes.Index scans the whole bzImage, so the 2-byte utils.GzipMagic would false-positive.
		{"gzip", []byte{0x1f, 0x8b, 0x08}, decompressGzip},
	}

	var tried []string
	for _, f := range formats {
		tried = append(tried, f.name)
		offset := bytes.Index(data, f.magic)
		if offset < 0 {
			continue
		}
		decompressed, err := f.decode(data[offset:])
		if err != nil || !bytes.HasPrefix(decompressed, elfMagic) {
			continue
		}
		return decompressed, nil
	}
	return nil, fmt.Errorf("no supported compression format found (tried %s)", strings.Join(tried, ", "))
}

func decompressZstd(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("new zstd reader: %w", err)
	}
	defer dec.Close()
	// DecodeAll may error on trailing data after the first frame; any decoded prefix is valid — caller validates via ELF magic.
	out, err := dec.DecodeAll(data, nil)
	if len(out) == 0 {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	return out, nil
}

func decompressGzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck
	return io.ReadAll(r)
}

func buildCmdline(storageConfigs []*types.StorageConfig, networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	// Top layer first for overlayfs lowerdir; FC quirks (reboot=k, i8042.noaux, 8250.nr_uarts=1) skip absent-hardware probes, and FC itself adds pci=off on the MMIO transport.
	layerDevs := hypervisor.ReverseLayers(storageConfigs, func(idx int, _ *types.StorageConfig) string { return DevPath(idx) })
	return hypervisor.BuildBaseCmdline(
		"console=ttyS0 reboot=k loglevel=3 i8042.noaux 8250.nr_uarts=1",
		strings.Join(layerDevs, ","),
		DevPath(len(layerDevs)),
		networkConfigs, vmName, dnsServers,
	)
}
