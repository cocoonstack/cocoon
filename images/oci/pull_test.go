package oci

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/images"
)

func TestCollectBootHexesAndBootFilesPresent(t *testing.T) {
	idx := &imageIndex{
		Images: map[string]*imageEntry{
			"one": {
				KernelLayer: images.NewDigest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
				InitrdLayer: images.NewDigest("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"),
			},
		},
	}

	hexes := collectBootHexes(idx)
	if len(hexes) != 2 {
		t.Fatalf("collectBootHexes() len = %d, want 2", len(hexes))
	}

	hasKernel, hasInitrd := bootFilesPresent([]pullLayerResult{
		{kernelPath: "/tmp/kernel"},
		{initrdPath: "/tmp/initrd"},
	})
	if !hasKernel || !hasInitrd {
		t.Fatalf("bootFilesPresent() = (%v, %v), want (true, true)", hasKernel, hasInitrd)
	}
}

func TestMoveBootFileAndIsUpToDate(t *testing.T) {
	root := t.TempDir()
	conf := NewConfig(root, 1)
	if err := conf.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs(): %v", err)
	}

	digestHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	src := filepath.Join(srcDir, "vmlinuz")
	if err := os.WriteFile(src, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	dst := conf.KernelPath(digestHex)
	if err := moveBootFile(src, dst, conf.BootDir(digestHex), 0, "kernel"); err != nil {
		t.Fatalf("moveBootFile(): %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("Stat(%s): %v", dst, err)
	}

	blobPath := conf.BlobPath(digestHex)
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(blob): %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("blob"), 0o644); err != nil {
		t.Fatalf("WriteFile(blob): %v", err)
	}
	initrdPath := conf.InitrdPath(digestHex)
	if err := os.MkdirAll(filepath.Dir(initrdPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(initrd): %v", err)
	}
	if err := os.WriteFile(initrdPath, []byte("initrd"), 0o644); err != nil {
		t.Fatalf("WriteFile(initrd): %v", err)
	}

	idx := &imageIndex{
		Images: map[string]*imageEntry{
			"demo": {
				Ref:            "demo",
				ManifestDigest: images.NewDigest(digestHex),
				Layers:         []layerEntry{{Digest: images.NewDigest(digestHex)}},
				KernelLayer:    images.NewDigest(digestHex),
				InitrdLayer:    images.NewDigest(digestHex),
				CreatedAt:      time.Now(),
			},
		},
	}

	if !isUpToDate(conf, idx, "demo", digestHex) {
		t.Fatalf("isUpToDate() = false, want true")
	}
}
