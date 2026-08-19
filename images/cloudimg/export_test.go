package cloudimg

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	metajson "github.com/cocoonstack/cocoon/meta/json"
)

func TestExportResolvesAliasAndDigest(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	conf := NewConfig(root, 0)
	engine, err := metajson.Open(conf.JSONNamespace())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	backend, err := New(ctx, root, 0, engine)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("qcow2-test-payload")
	sum := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(sum[:])
	if err := os.WriteFile(conf.BlobPath(digestHex), payload, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := backend.store.Update(ctx, func(idx *imageIndex) error {
		return writeIndexEntry(idx, conf, "ubuntu:24.04", digestHex)
	}); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"ubuntu:24.04", "sha256:" + digestHex} {
		stream, err := backend.Export(ctx, ref)
		if err != nil {
			t.Fatalf("Export(%q): %v", ref, err)
		}
		got, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close Export(%q): %v / %v", ref, readErr, closeErr)
		}
		if string(got) != string(payload) {
			t.Fatalf("Export(%q) = %q", ref, got)
		}
	}
}

func TestExportRejectsUnknownImage(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	conf := NewConfig(root, 0)
	engine, err := metajson.Open(conf.JSONNamespace())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	backend, err := New(ctx, root, 0, engine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Export(ctx, "missing:latest"); err == nil {
		t.Fatal("Export() unexpectedly succeeded")
	}
}
