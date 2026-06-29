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
	// FC needs an uncompressed ELF kernel.
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

// DevPath maps idx to vda..vdz, vdaa..vdaz, vdba..vdbz, ...
func DevPath(idx int) string {
	const letters = 26
	if idx < letters {
		return fmt.Sprintf("/dev/vd%c", 'a'+idx)
	}
	return fmt.Sprintf("/dev/vd%c%c", 'a'+(idx/letters)-1, 'a'+idx%letters)
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
	if _, statErr := os.Stat(vmlinuxPath); statErr == nil {
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
		name  string
		magic []byte
	}
	formats := []kernelCodec{
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
		{"gzip", []byte{0x1f, 0x8b, 0x08}},
	}

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
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("new zstd reader: %w", err)
	}
	defer dec.Close()
	// DecodeAll may error on trailing data after the first frame; any prefix
	// already in out is valid — caller validates via ELF magic.
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
	// Top layer first for overlayfs lowerdir; FC quirks (reboot=k, pci=off, i8042.noaux, 8250.nr_uarts=1) skip absent-hardware probes.
	layerDevs := hypervisor.ReverseLayers(storageConfigs, func(idx int, _ *types.StorageConfig) string { return DevPath(idx) })
	return hypervisor.BuildBaseCmdline(
		"console=ttyS0 reboot=k loglevel=3 pci=off i8042.noaux 8250.nr_uarts=1",
		strings.Join(layerDevs, ","),
		DevPath(len(layerDevs)),
		networkConfigs, vmName, dnsServers,
	)
}
