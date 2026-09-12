package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

func TestIsAlreadyInStateError(t *testing.T) {
	chPaused := `PUT http://localhost/api/v1/vm.pause → 500: ["Error from API","The VM could not be paused","Cannot pause VM","Failed to pause migratable component","Invalid transition: InvalidStateTransition(Paused, Paused)"]`
	chRunning := `PUT http://localhost/api/v1/vm.resume → 500: ["Error from API","Cannot resume VM","Failed","Invalid transition: InvalidStateTransition(Running, Running)"]`

	tests := []struct {
		name  string
		err   error
		state string
		want  bool
	}{
		{name: "paused paused match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "Paused", want: true},
		{name: "running running match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chRunning}, state: "Running", want: true},
		{name: "wrong state in match", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "Running", want: false},
		{name: "non-500 code", err: &utils.APIError{Code: http.StatusBadRequest, Message: chPaused}, state: "Paused", want: false},
		{name: "different transition (Created→Paused)", err: &utils.APIError{Code: http.StatusInternalServerError, Message: "InvalidStateTransition(Created, Paused)"}, state: "Paused", want: false},
		{name: "non-APIError", err: errors.New("dial unix: connection refused"), state: "Paused", want: false},
		{name: "nil error", err: nil, state: "Paused", want: false},
		{name: "wrapped APIError", err: fmt.Errorf("snapshot save: %w", &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}), state: "Paused", want: true},
		{name: "empty state", err: &utils.APIError{Code: http.StatusInternalServerError, Message: chPaused}, state: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyInStateError(tt.err, tt.state); got != tt.want {
				t.Errorf("isAlreadyInStateError(%v, %q) = %v, want %v", tt.err, tt.state, got, tt.want)
			}
		})
	}
}

func TestSaveConsolePTYWritesQueriedPath(t *testing.T) {
	runDir := t.TempDir()
	sockPath := serveVMInfo(t, "/dev/pts/7")

	saveConsolePTY(t.Context(), "vm1", runDir, sockPath, true)

	got, err := os.ReadFile(hypervisor.ConsolePTYPath(runDir))
	if err != nil {
		t.Fatalf("read console.pty: %v", err)
	}
	if string(got) != "/dev/pts/7" {
		t.Errorf("console.pty = %q, want %q", got, "/dev/pts/7")
	}
}

func TestSaveConsolePTYSkipsUEFI(t *testing.T) {
	runDir := t.TempDir()

	saveConsolePTY(t.Context(), "vm1", runDir, filepath.Join(runDir, "api.sock"), false)

	if utils.FileExists(hypervisor.ConsolePTYPath(runDir)) {
		t.Error("console.pty written for a UEFI boot")
	}
}

func TestConfirmVMMReadyAcceptsAnAnsweringVMM(t *testing.T) {
	sockPath := serveVMInfo(t, "/dev/pts/7")

	if err := confirmVMMReady(t.Context(), utils.NewSocketHTTPClient(sockPath)); err != nil {
		t.Fatalf("confirmVMMReady: %v", err)
	}
}

func TestConfirmVMMReadyRejectsASocketNothingServes(t *testing.T) {
	sockPath := serveVMInfo(t, "/dev/pts/7")
	if err := os.Remove(sockPath); err != nil {
		t.Fatalf("remove %s: %v", sockPath, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := confirmVMMReady(ctx, utils.NewSocketHTTPClient(sockPath))

	if err == nil {
		t.Fatal("confirmVMMReady accepted a VMM that never answered vm.info")
	}
	if !strings.Contains(err.Error(), "did not answer vm.info") {
		t.Errorf("err = %v, want the launch failure to name the unanswered vm.info", err)
	}
}

func serveVMInfo(t *testing.T, ptyPath string) string {
	t.Helper()

	sockDir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "api.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		resp := chVMInfoResponse{Config: chVMInfoConfig{Console: chRuntimeFile{Mode: "Pty", File: ptyPath}}}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode vm.info: %v", err)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })
	return sockPath
}
