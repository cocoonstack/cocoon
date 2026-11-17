//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyDriveFDs(t *testing.T) {
	dir := t.TempDir()
	dst := writeTestFile(t, dir, "clone-cow.raw")
	binds := [][2]string{{filepath.Join(dir, "src.raw"), dst}}

	if err := verifyDriveFDs(os.Getpid(), binds); err == nil {
		t.Fatal("want error while dst is not open")
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := verifyDriveFDs(os.Getpid(), binds); err != nil {
		t.Fatalf("verify with open fd: %v", err)
	}
}
