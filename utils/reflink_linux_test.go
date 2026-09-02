//go:build linux

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReflinkCopyCachesAnUnsupportedFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReflinkCopy(t.Context(), filepath.Join(dir, "dst"), src, NoSync); err != nil {
		t.Fatalf("ReflinkCopy: %v", err)
	}
	fs, ok := fsID(dir)
	if !ok {
		t.Fatal("statfs failed on the temp dir")
	}
	_, cached := noReflink.Load(fs)
	unsupported := reflinkUnsupported(tryFiclone(filepath.Join(dir, "probe"), src, NoSync))
	if cached != unsupported {
		t.Fatalf("cache says unsupported=%v, a fresh FICLONE says unsupported=%v", cached, unsupported)
	}
}
