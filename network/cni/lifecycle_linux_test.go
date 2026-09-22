package cni

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDeleteNetnsCollectsAnUnmountedEntry(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to write " + netnsBasePath)
	}
	if err := os.MkdirAll(netnsBasePath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", netnsBasePath, err)
	}
	name := "cocoon-stale-" + strconv.Itoa(os.Getpid())
	path := filepath.Join(netnsBasePath, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	if err := deleteNetns(t.Context(), name); err != nil {
		t.Fatalf("deleteNetns: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale netns entry survived: stat err = %v", err)
	}
}
