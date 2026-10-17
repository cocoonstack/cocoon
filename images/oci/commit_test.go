package oci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/images"
)

func TestCommitBlobsKeepsPublishedBlob(t *testing.T) {
	root := t.TempDir()
	conf := NewConfig(root, 1)
	if err := conf.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs(): %v", err)
	}
	digestHex := blobHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	for _, content := range []string{"first", "second"} {
		tmp := filepath.Join(root, content+".erofs")
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", tmp, err)
		}
		if err := commitBlobs(conf, []pullLayerResult{{digest: images.NewDigest(digestHex), erofsPath: tmp}}); err != nil {
			t.Fatalf("commitBlobs(%s): %v", content, err)
		}
	}

	got, err := os.ReadFile(conf.BlobPath(digestHex))
	if err != nil {
		t.Fatalf("ReadFile(blob): %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("published blob = %q, want %q", got, "first")
	}
}
