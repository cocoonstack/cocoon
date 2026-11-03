package hypervisor

import (
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/types"
)

func TestRunningCloneRecordStampsStartAtLaunch(t *testing.T) {
	created := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	launched := created.Add(30 * time.Second)
	orig := timeNow
	timeNow = func() time.Time { return launched }
	t.Cleanup(func() { timeNow = orig })
	b := &Backend{Typ: "test"}
	rec := &VMRecord{ID: "c", CreatedAt: created, CPUSet: "0-7", QueueCPUs: "0-3", RunDir: t.TempDir()}
	info := b.RunningCloneRecord(rec, &types.VMConfig{Name: "c"}, nil, types.NetSetup{})
	if !info.CreatedAt.Equal(created) || info.StartedAt == nil || !info.StartedAt.Equal(launched) || !info.UpdatedAt.Equal(launched) {
		t.Errorf("created %v started %v updated %v, want created %v started/updated %v", info.CreatedAt, info.StartedAt, info.UpdatedAt, created, launched)
	}
	if info.CPUSet != "0-7" || info.QueueCPUs != "0-3" {
		t.Errorf("placement not carried: cpuset %q queue_cpus %q", info.CPUSet, info.QueueCPUs)
	}
}

func TestRetargetCloneDisks(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/cow.raw", Serial: CowSerial, Role: types.StorageRoleCOW},
		{Path: "/old/data-vol.raw", Serial: "vol", Role: types.StorageRoleData},
		{Path: "/old/cidata.img", RO: true, Role: types.StorageRoleCidata},
	}
	if err := RetargetCloneDisks(configs, "/new/cow.raw", "/new"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/old/layer.erofs", "/new/cow.raw", "/new/data-vol.raw", "/old/cidata.img"}
	for i, sc := range configs {
		if sc.Path != want[i] {
			t.Errorf("disk[%d] (%s): path %s, want %s", i, sc.Role, sc.Path, want[i])
		}
	}
}

func TestRetargetCloneDisksNoCOW(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/data-vol.raw", Serial: "vol", Role: types.StorageRoleData},
	}
	if err := RetargetCloneDisks(configs, "/new/cow.raw", "/new"); err == nil {
		t.Fatal("expected error when no COW role disk")
	}
}
