package firecracker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestRedirectedDriveIndices(t *testing.T) {
	sc := func(paths ...string) []*types.StorageConfig {
		configs := make([]*types.StorageConfig, len(paths))
		for i, p := range paths {
			configs[i] = &types.StorageConfig{Path: p}
		}
		return configs
	}
	tests := []struct {
		name     string
		src, dst []*types.StorageConfig
		want     []int
	}{
		{"all equal", sc("/a", "/b"), sc("/a", "/b"), nil},
		{"one differs", sc("/a", "/b", "/c"), sc("/a", "/B", "/c"), []int{1}},
		{"all differ", sc("/a", "/b"), sc("/A", "/B"), []int{0, 1}},
		{"dst shorter", sc("/a", "/b"), sc("/A"), []int{0}},
		{"empty", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redirectedDriveIndices(tt.src, tt.dst); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// The redirect set and the re-anchor set must be the same drives: both loops
// derive from redirectedDriveIndices, and this pins createDriveRedirects to
// it — a symlink appears exactly where the shared function says.
func TestCreateDriveRedirectsMatchesIndices(t *testing.T) {
	dir := t.TempDir()
	src := []*types.StorageConfig{
		{Path: filepath.Join(dir, "gone", "layer.img")}, // shared layer, same both sides
		{Path: filepath.Join(dir, "gone", "cow.raw")},   // source path no longer exists
		{Path: writeTestFile(t, dir, "data.img")},       // source still present: backed up
	}
	dst := []*types.StorageConfig{
		{Path: src[0].Path},
		{Path: writeTestFile(t, dir, "clone-cow.raw")},
		{Path: writeTestFile(t, dir, "clone-data.img")},
	}

	redirects, err := createDriveRedirects(src, dst)
	if err != nil {
		t.Fatalf("createDriveRedirects: %v", err)
	}
	defer cleanupDriveRedirects(redirects)

	want := redirectedDriveIndices(src, dst)
	if len(redirects) != len(want) {
		t.Fatalf("%d redirects for indices %v", len(redirects), want)
	}
	for n, i := range want {
		if redirects[n].symlinkPath != src[i].Path {
			t.Errorf("redirect %d at %q, want %q", n, redirects[n].symlinkPath, src[i].Path)
		}
		target, readErr := os.Readlink(src[i].Path)
		if readErr != nil {
			t.Fatalf("drive %d not symlinked: %v", i, readErr)
		}
		if target != dst[i].Path {
			t.Errorf("drive %d links to %q, want %q", i, target, dst[i].Path)
		}
	}
}

func TestCleanupDriveRedirectsRemovesRecreatedDir(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	src := []*types.StorageConfig{
		{Path: filepath.Join(gone, "cow.raw")},
		{Path: filepath.Join(gone, "data-x.raw")},
	}
	dst := []*types.StorageConfig{
		{Path: writeTestFile(t, dir, "clone-cow.raw")},
		{Path: writeTestFile(t, dir, "clone-data-x.raw")},
	}

	redirects, err := createDriveRedirects(src, dst)
	if err != nil {
		t.Fatalf("createDriveRedirects: %v", err)
	}
	cleanupDriveRedirects(redirects)

	if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("recreated source dir not removed on unwind: %v", err)
	}
}

func writeTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
		t.Fatalf("setup %s: %v", name, err)
	}
	return p
}
