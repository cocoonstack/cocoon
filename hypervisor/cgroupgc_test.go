package hypervisor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cocoonstack/cocoon/cgroup"
)

func TestCgroupGCModuleSweepsUnownedScopes(t *testing.T) {
	parent := t.TempDir()
	for _, id := range []string{"OWNED-CH", "OWNED-FC", "ORPHAN"} {
		if err := os.Mkdir(cgroup.ScopeDir(parent, id), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	m := CgroupGCModule(parent)
	scopes, err := m.ReadDB(t.Context())
	if err != nil {
		t.Fatalf("ReadDB: %v", err)
	}
	slices.Sort(scopes)
	if want := []string{"ORPHAN", "OWNED-CH", "OWNED-FC"}; !slices.Equal(scopes, want) {
		t.Fatalf("scopes = %v, want %v", scopes, want)
	}

	others := map[string]any{
		"cloud-hypervisor": VMGCSnapshot{vmIDs: map[string]struct{}{"OWNED-CH": {}}},
		"firecracker":      VMGCSnapshot{vmIDs: map[string]struct{}{"OWNED-FC": {}}},
	}
	ids := m.Resolve(t.Context(), scopes, others)
	if want := []string{"ORPHAN"}; !slices.Equal(ids, want) {
		t.Fatalf("Resolve = %v, want %v", ids, want)
	}

	if err := m.Collect(t.Context(), ids, nil); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := os.Stat(cgroup.ScopeDir(parent, "ORPHAN")); !os.IsNotExist(err) {
		t.Error("orphan scope not removed")
	}
	for _, id := range []string{"OWNED-CH", "OWNED-FC"} {
		if _, err := os.Stat(cgroup.ScopeDir(parent, id)); err != nil {
			t.Errorf("owned scope %s removed: %v", id, err)
		}
	}
}

func TestCgroupGCModuleLeavesPopulatedScope(t *testing.T) {
	parent := t.TempDir()
	dir := cgroup.ScopeDir(parent, "BUSY")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m := CgroupGCModule(parent)
	if err := m.Collect(t.Context(), []string{"BUSY"}, nil); err == nil {
		t.Error("want error for populated scope, got nil")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("populated scope removed: %v", err)
	}
}
