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
