package firecracker

import (
	"bytes"
	"compress/gzip"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestDevPath(t *testing.T) {
	tests := []struct {
		idx  int
		want string
	}{
		{0, "/dev/vda"},
		{1, "/dev/vdb"},
		{25, "/dev/vdz"},
		{26, "/dev/vdaa"},
		{27, "/dev/vdab"},
		{51, "/dev/vdaz"},
		{52, "/dev/vdba"},
		{77, "/dev/vdbz"},
		{78, "/dev/vdca"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := DevPath(tt.idx)
			if got != tt.want {
				t.Errorf("DevPath(%d) = %q, want %q", tt.idx, got, tt.want)
			}
		})
	}
}

func TestDecompressKernel(t *testing.T) {
	elf := fakeELF()

	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{
			name: "gzip",
			data: gzipCompress(t, elf),
		},
		{
			name: "zstd",
			data: zstdCompress(t, elf),
		},
		{
			name: "gzip with header prefix (bzImage-style)",
			data: append(bytes.Repeat([]byte{0xff}, 64), gzipCompress(t, elf)...),
		},
		{
			name: "zstd with header prefix (bzImage-style)",
			data: append(bytes.Repeat([]byte{0xff}, 64), zstdCompress(t, elf)...),
		},
		{
			name:    "no recognized magic",
			data:    bytes.Repeat([]byte{0xff}, 128),
			wantErr: true,
		},
		{
			name:    "gzip header but corrupt payload",
			data:    append([]byte{0x1f, 0x8b, 0x08}, bytes.Repeat([]byte{0xff}, 32)...),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decompressKernel(tt.data)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !bytes.HasPrefix(got, []byte{0x7f, 'E', 'L', 'F'}) {
				t.Errorf("output does not start with ELF magic: %x", got[:min(16, len(got))])
			}
		})
	}
}

func TestCreateRejectsUnsupportedMemoryConfig(t *testing.T) {
	fc := &Firecracker{}
	tests := []struct {
		name string
		cfg  types.Config
		want string
	}{
		{"shared memory", types.Config{SharedMemory: true}, "shared memory"},
		{"hugepages", types.Config{HugePages: true}, "hugepages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fc.Create(t.Context(), "vm1", &types.VMConfig{Config: tt.cfg}, nil, types.NetSetup{}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want a %q rejection", err, tt.want)
			}
		})
	}
}

func TestBuildCmdlineLeavesPCIToFirecracker(t *testing.T) {
	cmdline := buildCmdline([]*types.StorageConfig{{Role: types.StorageRoleLayer}}, nil, "vm", nil)
	if strings.Contains(cmdline, "pci=off") {
		t.Errorf("cmdline carries pci=off, which Firecracker adds itself on MMIO and must not see on PCI: %q", cmdline)
	}
}

func TestLaunchArgsEnablePCI(t *testing.T) {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1"}}
	if args := launchArgs(rec, "/run/api.sock", "/log/fc.log"); slices.Contains(args, "--enable-pci") {
		t.Errorf("MMIO launch passes --enable-pci: %v", args)
	}
	rec.Config.PCI = true
	if args := launchArgs(rec, "/run/api.sock", "/log/fc.log"); !slices.Contains(args, "--enable-pci") {
		t.Errorf("PCI launch lacks --enable-pci: %v", args)
	}
}

func fakeELF() []byte {
	out := []byte{0x7f, 'E', 'L', 'F'}
	out = append(out, bytes.Repeat([]byte{0x00}, 60)...)
	return out
}

func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	out := enc.EncodeAll(data, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return out
}
