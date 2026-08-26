package hypervisor

import (
	"os"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestToVMClearsPersistedRuntimeFieldsWhenStopped(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedVMRecord(t, b, "vm1", 1, 1<<30, 10<<30, true)
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs["vm1"].State = types.VMStateStopped
		idx.VMs["vm1"].SocketPath = "/stale/api.sock"
		idx.VMs["vm1"].VsockSocket = "/stale/vsock.uds"
		idx.VMs["vm1"].ConsolePath = "/dev/pts/9"
		return nil
	}); err != nil {
		t.Fatalf("seed runtime fields: %v", err)
	}

	rec, err := b.LoadRecord(t.Context(), "vm1")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	info := b.ToVM(&rec)
	if info.SocketPath != "" || info.VsockSocket != "" || info.ConsolePath != "" {
		t.Errorf("stopped VM leaks runtime paths: socket=%q vsock=%q console=%q",
			info.SocketPath, info.VsockSocket, info.ConsolePath)
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
			rec, err := b.LoadRecord(t.Context(), "vm1")
			if err != nil {
				t.Fatalf("load record: %v", err)
			}
			tt.setup(t, rec.RunDir)

			info := b.ToVM(&rec)
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
