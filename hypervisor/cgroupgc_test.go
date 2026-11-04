package hypervisor

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"syscall"
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

	m := CgroupGCModule(parent, noVMInUse)
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

func TestCgroupGCModuleLeavesScopeOfAVMCreatedAfterTheSnapshot(t *testing.T) {
	parent := t.TempDir()
	dir := cgroup.ScopeDir(parent, "LAUNCHING")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m := CgroupGCModule(parent, func(_ context.Context, id string) (bool, error) { return id == "LAUNCHING", nil })
	if err := m.Collect(t.Context(), []string{"LAUNCHING"}, nil); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("empty scope of a VM whose record landed after the snapshot removed: %v", err)
	}
}

func TestCgroupGCModuleCollectRemoveFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		errno   syscall.Errno
		wantErr bool
	}{
		{name: "kernel refuses a populated scope", errno: syscall.EBUSY},
		{name: "any other refusal surfaces", errno: syscall.ENOTEMPTY, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orig := removeScope
			removeScope = func(string, string) error {
				return &fs.PathError{Op: "remove", Path: "vm-BUSY.scope", Err: tt.errno}
			}
			t.Cleanup(func() { removeScope = orig })

			err := CgroupGCModule(t.TempDir(), noVMInUse).Collect(t.Context(), []string{"BUSY"}, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("Collect = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func noVMInUse(context.Context, string) (bool, error) { return false, nil }
