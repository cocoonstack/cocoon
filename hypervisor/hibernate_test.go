package hypervisor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type hibernateCalls struct {
	resumed    bool
	terminated bool
}

func TestHibernateSequencePersistFailureResumesAndRollsBack(t *testing.T) {
	b, id := newHibernateTestVM(t)
	calls := &hibernateCalls{}

	err := b.HibernateSequence(t.Context(), id, hibernateStubSpec(calls), func(_ *types.SnapshotConfig, stream io.ReadCloser) error {
		_, _ = io.Copy(io.Discard, stream)
		_ = stream.Close()
		return errors.New("store full")
	})
	if err == nil || !strings.Contains(err.Error(), "persist snapshot") {
		t.Fatalf("HibernateSequence: %v, want persist snapshot error", err)
	}
	if !calls.resumed {
		t.Error("VM was not resumed after persist failure")
	}
	if calls.terminated {
		t.Error("VMM was terminated despite persist failure")
	}

	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != types.VMStateRunning {
		t.Errorf("state %s, want running", rec.State)
	}
	if len(rec.SnapshotIDs) != 0 {
		t.Errorf("snapshot ids not rolled back: %v", rec.SnapshotIDs)
	}
}

func TestHibernateSequenceSuccess(t *testing.T) {
	b, id := newHibernateTestVM(t)
	calls := &hibernateCalls{}

	var gotCfg *types.SnapshotConfig
	err := b.HibernateSequence(t.Context(), id, hibernateStubSpec(calls), func(cfg *types.SnapshotConfig, stream io.ReadCloser) error {
		gotCfg = cfg
		_, _ = io.Copy(io.Discard, stream)
		return stream.Close()
	})
	if err != nil {
		t.Fatalf("HibernateSequence: %v", err)
	}
	if !calls.terminated {
		t.Error("VMM was not terminated")
	}
	if calls.resumed {
		t.Error("VM was resumed on the success path")
	}
	if gotCfg == nil || gotCfg.ID == "" {
		t.Fatalf("persist got cfg %+v, want a snapshot id", gotCfg)
	}

	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != types.VMStateStopped {
		t.Errorf("state %s, want stopped", rec.State)
	}
	if _, ok := rec.SnapshotIDs[gotCfg.ID]; !ok {
		t.Errorf("snapshot id %s not recorded: %v", gotCfg.ID, rec.SnapshotIDs)
	}
}

func TestHibernateSequenceTerminateFailureMarksError(t *testing.T) {
	b, id := newHibernateTestVM(t)
	calls := &hibernateCalls{}

	spec := hibernateStubSpec(calls)
	spec.Terminate = func(*VMRecord, *http.Client, int) error { return errors.New("kill refused") }
	err := b.HibernateSequence(t.Context(), id, spec, func(_ *types.SnapshotConfig, stream io.ReadCloser) error {
		_, _ = io.Copy(io.Discard, stream)
		return stream.Close()
	})
	if err == nil || !strings.Contains(err.Error(), "terminate") {
		t.Fatalf("HibernateSequence: %v, want terminate error", err)
	}
	if calls.resumed {
		t.Error("VM was resumed after the snapshot was already persisted")
	}

	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if rec.State != types.VMStateError {
		t.Errorf("state %s, want error", rec.State)
	}
	if len(rec.SnapshotIDs) != 1 {
		t.Errorf("persisted snapshot id missing from record: %v", rec.SnapshotIDs)
	}
}

// newHibernateTestVM seeds a running VM whose RunDir holds a live stub VMM process, so WithRunningVM lets the sequence proceed.
func newHibernateTestVM(t *testing.T) (*Backend, string) {
	t.Helper()
	b, _ := newMeteringTestBackend(t)
	const id = "vm-hibernate-test"
	runDir := shortTempDir(t)
	seedVMRecord(t, b, id, 1, 512, 1024, true)
	if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].State = types.VMStateRunning
		idx.VMs[id].RunDir = runDir
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// argv[0] and the socket-path argument satisfy WithRunningVM's cmdline check.
	sock := SocketPath(runDir)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("touch sock: %v", err)
	}
	cmd := exec.Command("bash", "-c", fmt.Sprintf("exec -a %s tail -f /dev/null %q", b.Conf.BinaryName(), sock))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub vmm: %v", err)
	}
	// Background reaper: a test-killed stub must not linger as a zombie, or
	// TerminateProcess's IsProcessAlive polling never sees it exit.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waitDone
	})
	// cmd.Start returns before bash execs into tail; wait until the cmdline check would pass.
	deadline := time.Now().Add(2 * time.Second)
	for !utils.VerifyProcessCmdline(cmd.Process.Pid, b.Conf.BinaryName(), sock) {
		if time.Now().After(deadline) {
			t.Fatal("stub vmm did not exec in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(b.PIDFilePath(runDir), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	return b, id
}

func hibernateStubSpec(calls *hibernateCalls) HibernateSpec {
	return HibernateSpec{
		SnapshotSpec: SnapshotSpec{
			Pause:  func(*VMRecord, *http.Client) error { return nil },
			Resume: func(*VMRecord, *http.Client) error { calls.resumed = true; return nil },
			Capture: func(_ *VMRecord, _ *http.Client, tmpDir string) error {
				return os.WriteFile(filepath.Join(tmpDir, "mem"), []byte("x"), 0o600)
			},
			BuildMeta: func(*VMRecord, string) (*SnapshotMeta, error) { return &SnapshotMeta{}, nil },
		},
		Terminate: func(*VMRecord, *http.Client, int) error { calls.terminated = true; return nil },
	}
}
