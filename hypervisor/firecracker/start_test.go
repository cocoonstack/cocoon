package firecracker

import (
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestStartCompletesInterruptedConfiguration(t *testing.T) {
	for _, state := range []types.VMState{types.VMStateCreated, types.VMStateRunning} {
		t.Run(string(state), func(t *testing.T) {
			fc := newTestFC(t)
			const id = "interrupted-start"
			runDir, err := os.MkdirTemp("/tmp", "fc-start-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runDir) })
			cfg := &types.VMConfig{Name: id, CPU: 1, Memory: 128 << 20, NoBalloon: true}
			if err := fc.ReserveVM(t.Context(), id, cfg, nil, runDir, runDir); err != nil {
				t.Fatal(err)
			}
			if err := fc.UpdateRecord(t.Context(), id, func(rec *hypervisor.VMRecord) error {
				rec.State = state
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			liveState := "Not started"
			fail := true
			var puts []string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet && r.URL.Path == "/" {
					_ = json.MarshalWrite(w, fcInstanceInfo{State: liveState})
					return
				}
				if r.Method != http.MethodPut || liveState != "Not started" {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				puts = append(puts, r.URL.Path)
				if fail && r.URL.Path == "/vsock" {
					http.Error(w, "configuration failed", http.StatusBadRequest)
					return
				}
				if r.URL.Path == "/actions" {
					var action fcAction
					if err := json.UnmarshalRead(r.Body, &action); err != nil || action.ActionType != actionInstanceStart {
						http.Error(w, "expected InstanceStart", http.StatusBadRequest)
						return
					}
					liveState = "Running"
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			_ = srv.Listener.Close()
			sock := hypervisor.SocketPath(runDir)
			srv.Listener, err = net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			srv.Start()
			t.Cleanup(srv.Close)
			cmd := exec.Command("bash", "-c", fmt.Sprintf("exec -a %s tail -f /dev/null %q", fc.Conf.BinaryName(), sock))
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})
			if err := utils.WaitFor(t.Context(), 2*time.Second, 10*time.Millisecond, func() (bool, error) {
				return utils.VerifyProcessCmdline(cmd.Process.Pid, fc.Conf.BinaryName(), sock), nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := utils.WritePIDFile(fc.PIDFilePath(runDir), cmd.Process.Pid); err != nil {
				t.Fatal(err)
			}

			if err := fc.startOne(t.Context(), id, nil); err == nil {
				t.Fatal("start succeeded without completing configuration")
			}
			rec, err := fc.LoadRecord(t.Context(), id)
			if err != nil || rec.State != state {
				t.Fatalf("failed start: state=%s err=%v, want %s", rec.State, err, state)
			}
			mu.Lock()
			fail = false
			mu.Unlock()
			for range 2 {
				if err := fc.startOne(t.Context(), id, nil); err != nil {
					t.Fatalf("retry start: %v", err)
				}
			}
			rec, err = fc.LoadRecord(t.Context(), id)
			if err != nil || rec.State != types.VMStateRunning {
				t.Fatalf("completed start: state=%s err=%v, want running", rec.State, err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/machine-config", "/entropy", "/vsock", "/machine-config", "/entropy", "/vsock", "/actions"}
			if liveState != "Running" || !slices.Equal(puts, want) {
				t.Fatalf("state=%s requests=%v, want running and %v", liveState, puts, want)
			}
		})
	}
}
