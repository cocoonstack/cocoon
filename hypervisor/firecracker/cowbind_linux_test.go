//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyDriveFDs(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "clone-cow.raw")
	if err := os.WriteFile(dst, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	binds := [][2]string{{filepath.Join(dir, "src.raw"), dst}}

	if err := verifyDriveFDs(os.Getpid(), binds); err == nil {
		t.Fatal("want error while dst is not open")
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if err := verifyDriveFDs(os.Getpid(), binds); err != nil {
		t.Fatalf("verify with open fd: %v", err)
	}
}
