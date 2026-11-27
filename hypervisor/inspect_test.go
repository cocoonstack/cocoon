package hypervisor

import (
	"os"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestToVMClearsPersistedRuntimeFieldsWhenStopped(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)
	if err := b.UpdateRecord(t.Context(), "vm1", func(r *VMRecord) error {
		r.State = types.VMStateStopped
		r.SocketPath = "/stale/api.sock"
		r.VsockSocket = "/stale/vsock.uds"
		r.ConsolePath = "/dev/pts/9"
		r.PID = 4242
		return nil
	}); err != nil {
		t.Fatalf("seed runtime fields: %v", err)
	}

	rec := recordOf(t, b, "vm1")
	info := b.ToVM(rec)
	if info.PID != 0 || info.SocketPath != "" || info.VsockSocket != "" || info.ConsolePath != "" {
		t.Errorf("stopped VM leaks runtime fields: pid=%d socket=%q vsock=%q console=%q",
			info.PID, info.SocketPath, info.VsockSocket, info.ConsolePath)
	}
}

func TestSetSize(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedVMRecord(t, b, "stopped", 2, 4<<30, 20<<30, true)
	seedRunningVM(t, b, "running", 2, 4<<30, 20<<30)

	if err := b.SetSize(t.Context(), "stopped", 4, 8<<30); err != nil {
		t.Fatalf("SetSize stopped: %v", err)
	}
	if rec := recordOf(t, b, "stopped"); rec.Config.CPU != 4 || rec.Config.Memory != 8<<30 {
		t.Errorf("stopped VM size = %d/%d, want 4/%d", rec.Config.CPU, rec.Config.Memory, int64(8<<30))
	}

	if err := b.SetSize(t.Context(), "running", 4, 8<<30); err == nil {
		t.Error("SetSize on a running VM succeeded")
	}
	if rec := recordOf(t, b, "running"); rec.Config.CPU != 2 || rec.Config.Memory != 4<<30 {
		t.Errorf("running VM size = %d/%d, want unchanged 2/%d", rec.Config.CPU, rec.Config.Memory, int64(4<<30))
	}
}

func TestToVMRunningConsolePath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, runDir string)
		want  func(runDir string) string
	}{
		{
			name:  "console sock",
			setup: func(t *testing.T, runDir string) { writeRunFile(t, ConsoleSockPath(runDir), "") },
			want:  ConsoleSockPath,
		},
		{
			name:  "pty file",
			setup: func(t *testing.T, runDir string) { writeRunFile(t, ConsolePTYPath(runDir), "/dev/pts/5\n") },
			want:  func(string) string { return "/dev/pts/5" },
		},
		{
			name:  "no console",
			setup: func(*testing.T, string) {},
			want:  func(string) string { return "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newMeteringTestBackend(t)
			seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)
			rec := recordOf(t, b, "vm1")
			tt.setup(t, rec.RunDir)

			info := b.ToVM(rec)
			if want := tt.want(rec.RunDir); info.ConsolePath != want {
				t.Errorf("ConsolePath = %q, want %q", info.ConsolePath, want)
			}
			if info.SocketPath != SocketPath(rec.RunDir) {
				t.Errorf("SocketPath = %q, want %q", info.SocketPath, SocketPath(rec.RunDir))
			}
		})
	}
}

func writeRunFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
