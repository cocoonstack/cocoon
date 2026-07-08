package hypervisor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestFailRestoreSparesStoppedOrigin(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	tests := []struct {
		name   string
		origin types.VMState
		want   types.VMState
	}{
		{"stopped origin stays stopped (wake retryable)", types.VMStateStopped, types.VMStateStopped},
		{"running origin marks error", types.VMStateRunning, types.VMStateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "vm-fail-" + string(tt.origin)
			seedVMRecord(t, b, id, 1, 512, 1024, true)
			if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
				idx.VMs[id].State = tt.origin
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}

			b.FailRestore(t.Context(), id, tt.origin)

			rec, err := b.LoadRecord(t.Context(), id)
			if err != nil {
				t.Fatalf("load record: %v", err)
			}
			if rec.State != tt.want {
				t.Errorf("state %s, want %s", rec.State, tt.want)
			}
		})
	}
}

func TestDirectRestoreFailureKeepsOriginContract(t *testing.T) {
	failAfterExtract := func(spec *DirectRestoreSpec) {
		spec.AfterExtract = func(context.Context, string, *types.VMConfig, *VMRecord) (*types.VM, error) {
			return nil, errors.New("launch boom")
		}
	}
	failPopulate := func(spec *DirectRestoreSpec) {
		spec.Populate = func(*VMRecord, string) error { return errors.New("populate boom") }
	}
	tests := []struct {
		name   string
		origin types.VMState
		fail   func(*DirectRestoreSpec)
		want   types.VMState
	}{
		{"stopped origin survives a post-populate failure", types.VMStateStopped, failAfterExtract, types.VMStateStopped},
		{"running origin quarantines on a post-populate failure", types.VMStateRunning, failAfterExtract, types.VMStateError},
		{"partial populate quarantines even a stopped origin", types.VMStateStopped, failPopulate, types.VMStateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newMeteringTestBackend(t)
			const id = "vm-direct-fail"
			seedVMRecord(t, b, id, 1, 512, 1024, true)
			if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
				idx.VMs[id].State = tt.origin
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}

			spec := DirectRestoreSpec{
				VMCfg:     &types.VMConfig{Config: types.Config{CPU: 1, Memory: 512, Storage: 1024}},
				SrcDir:    t.TempDir(),
				Preflight: func(string, *VMRecord) error { return nil },
				Kill:      func(context.Context, string, *VMRecord) error { return nil },
				Populate:  func(*VMRecord, string) error { return nil },
			}
			tt.fail(&spec)
			if _, err := b.DirectRestoreSequence(t.Context(), id, spec); err == nil {
				t.Fatal("expected restore failure")
			}
			rec, err := b.LoadRecord(t.Context(), id)
			if err != nil {
				t.Fatalf("load record: %v", err)
			}
			if rec.State != tt.want {
				t.Errorf("state %s, want %s", rec.State, tt.want)
			}
		})
	}
}

func TestRestorePartialMergeQuarantinesEvenStoppedOrigin(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	const id = "vm-merge-fail"
	runDir := t.TempDir()
	seedVMRecord(t, b, id, 1, 512, 1024, true)
	if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateStopped
		idx.VMs[id].RunDir = runDir
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// Staged "a" merges, then staged file "b" cannot rename over the run
	// dir's directory "b": the merge fails halfway through, which must
	// quarantine even a stopped origin.
	if err := os.MkdirAll(filepath.Join(runDir, "b"), 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	spec := RestoreSpec{
		VMCfg:     &types.VMConfig{Config: types.Config{CPU: 1, Memory: 512, Storage: 1024}},
		Snapshot:  tarWithFiles(t, "a", "b"),
		Preflight: func(string, *VMRecord) error { return nil },
		Kill:      func(context.Context, string, *VMRecord) error { return nil },
		AfterExtract: func(context.Context, string, *types.VMConfig, *VMRecord) (*types.VM, error) {
			t.Fatal("AfterExtract must not run after a failed merge")
			return nil, nil
		},
	}
	if _, err := b.RestoreSequence(t.Context(), id, spec); err == nil {
		t.Fatal("expected merge failure")
	}
	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != types.VMStateError {
		t.Errorf("state %s, want error (mixed run dir must quarantine)", rec.State)
	}
	if _, err := os.Stat(filepath.Join(runDir, "a")); err != nil {
		t.Errorf("merge was not partial: %v", err)
	}
}

func tarWithFiles(t *testing.T, names ...string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1}); err != nil {
			t.Fatalf("tar hdr %s: %v", name, err)
		}
		if _, err := tw.Write([]byte("y")); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return &buf
}

func TestResolveForRestoreStates(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	tests := []struct {
		name    string
		state   types.VMState
		wantErr string
	}{
		{"running restorable", types.VMStateRunning, ""},
		{"stopped restorable (hibernate resume)", types.VMStateStopped, ""},
		{"error rejected", types.VMStateError, "must be running or stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "vm-" + string(tt.state)
			seedVMRecord(t, b, id, 1, 512, 1024, true)
			if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
				idx.VMs[id].State = tt.state
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}

			_, rec, err := b.ResolveForRestore(t.Context(), id)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveForRestore: %v", err)
				}
				if rec.State != tt.state {
					t.Errorf("state %s, want %s", rec.State, tt.state)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestKillForRestoreFailureKeepsOriginContract(t *testing.T) {
	tests := []struct {
		name   string
		origin types.VMState
		want   types.VMState
	}{
		{"stopped origin stays restorable (hibernate wake)", types.VMStateStopped, types.VMStateStopped},
		{"running origin quarantines", types.VMStateRunning, types.VMStateError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, id := newHibernateTestVM(t)
			if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
				idx.VMs[id].State = tt.origin
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			rec, err := b.LoadRecord(t.Context(), id)
			if err != nil {
				t.Fatalf("load record: %v", err)
			}

			killErr := b.KillForRestore(t.Context(), id, &rec, func(int) error { return errors.New("kill refused") }, nil)
			if killErr == nil || !strings.Contains(killErr.Error(), "stop running VM") {
				t.Fatalf("KillForRestore: %v, want stop running VM error", killErr)
			}

			got, err := b.LoadRecord(t.Context(), id)
			if err != nil {
				t.Fatalf("reload record: %v", err)
			}
			if got.State != tt.want {
				t.Errorf("state %s, want %s", got.State, tt.want)
			}
		})
	}
}

// A corrupt persisted record (null storage/NIC entry) must fail restore preflight
// with a clean invariants error, not panic in ValidateMetaPaths/ValidateRoleSequence.
func TestPrepareRestoreRejectsCorruptRecord(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VMRecord)
		wantErr string
	}{
		{"nil storage config", func(r *VMRecord) { r.StorageConfigs = []*types.StorageConfig{nil} }, "storage invariants violated"},
		{"nil network config", func(r *VMRecord) {
			r.StorageConfigs = nil
			r.NetworkConfigs = []*types.NetworkConfig{nil}
		}, "network invariants violated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newMeteringTestBackend(t)
			ctx := t.Context()
			const id = "vm-corrupt"
			seedStoppedVMWithDirs(t, b, id)
			if err := b.DB.Update(ctx, func(idx *VMIndex) error {
				tt.mutate(idx.VMs[id])
				return nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, _, unlock, err := b.prepareRestore(ctx, id)
			if unlock != nil {
				unlock()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
