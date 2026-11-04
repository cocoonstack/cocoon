//go:build linux

package utils

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

func TestScanProcsByBinaryReadErrors(t *testing.T) {
	self := os.Getpid()
	selfPath := fmt.Sprintf("/proc/%d/cmdline", self)
	injected := errors.New("injected read failure")
	failAll := func(path string) ([]byte, error) {
		if path == selfPath {
			return nil, injected
		}
		return nil, os.ErrNotExist
	}

	tests := []struct {
		name    string
		alive   aliveFn
		wantErr error
	}{
		{"read error on live process fails closed", func(pid int) bool { return pid == self }, injected},
		{"read error on vanished process is skipped", func(int) bool { return false }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := scanProcsByBinary("no-such-binary", failAll, tt.alive); !errors.Is(err, tt.wantErr) {
				t.Errorf("scanProcsByBinary: got err %v, want %v", err, tt.wantErr)
			}
		})
	}

	t.Run("matches survive skipped entries", func(t *testing.T) {
		readFile := func(path string) ([]byte, error) {
			if path == selfPath {
				return []byte("/bin/testvmm\x00--api-socket\x00/run/x.sock"), nil
			}
			return nil, os.ErrNotExist
		}
		scan, err := scanProcsByBinary("testvmm", readFile, func(int) bool { return false })
		if err != nil {
			t.Fatalf("scanProcsByBinary: %v", err)
		}
		if got := scan.Find("/run/x.sock"); !slices.Equal(got, []int{self}) {
			t.Errorf("Find: got %v, want [%d]", got, self)
		}
	})
}

func TestScanProcsByBinarySkipsKthreaddChildren(t *testing.T) {
	self := os.Getpid()
	selfPath := fmt.Sprintf("/proc/%d/cmdline", self)
	const kthreadd = "2 (kthreadd) S 0 0 0 0 -1 2129984 0 0"

	tests := []struct {
		name     string
		children string
		want     []int
	}{
		{"listed pid is not opened", fmt.Sprintf("%d ", self), nil},
		{"unlisted pid is matched", fmt.Sprintf("%d ", self+1), []int{self}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readFile := fakeProc(map[string]string{
				"/proc/2/stat":            kthreadd,
				"/proc/2/task/2/children": tt.children,
				selfPath:                  "/bin/testvmm\x00--api-socket\x00/run/x.sock",
			})
			scan, err := scanProcsByBinary("testvmm", readFile, func(int) bool { return false })
			if err != nil {
				t.Fatalf("scanProcsByBinary: %v", err)
			}
			if got := scan.Find("/run/x.sock"); !slices.Equal(got, tt.want) {
				t.Errorf("Find: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadKthreaddChildren(t *testing.T) {
	const children = "3 4 x 5 \n"

	tests := []struct {
		name string
		proc map[string]string
		want []int
	}{
		{"kthreadd children", map[string]string{"/proc/2/stat": "2 (kthreadd) S 0 0 0 0 -1 2129984 0", "/proc/2/task/2/children": children}, []int{3, 4, 5}},
		{"comm with a paren", map[string]string{"/proc/2/stat": "2 (a) b) S 0 0 0 0 -1 2129984 0", "/proc/2/task/2/children": children}, []int{3, 4, 5}},
		{"pid namespace process", map[string]string{"/proc/2/stat": "2 (sh) S 1 1 1 0 -1 4194560 0", "/proc/2/task/2/children": children}, nil},
		{"no stat", map[string]string{"/proc/2/task/2/children": children}, nil},
		{"truncated stat", map[string]string{"/proc/2/stat": "2 (kthreadd) S 0", "/proc/2/task/2/children": children}, nil},
		{"bad flags", map[string]string{"/proc/2/stat": "2 (kthreadd) S 0 0 0 0 -1 x 0", "/proc/2/task/2/children": children}, nil},
		{"no children file", map[string]string{"/proc/2/stat": "2 (kthreadd) S 0 0 0 0 -1 2129984 0"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(readKthreaddChildren(fakeProc(tt.proc))))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadKthreaddChildrenExcludesSelf(t *testing.T) {
	if _, listed := readKthreaddChildren(os.ReadFile)[os.Getpid()]; listed {
		t.Errorf("own pid %d listed as a kthreadd child", os.Getpid())
	}
}

func TestVerifyProcessCmdlineIgnoresTheBinaryName(t *testing.T) {
	const sock = "/run/vm/api.sock"
	cmd := exec.Command("/bin/sh", "-c", "sleep 60; :", "sh", sock)
	cmd.Args[0] = "a-renamed-vmm"
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := WaitFor(t.Context(), 5*time.Second, time.Millisecond, func() (bool, error) {
		return VerifyProcessCmdline(cmd.Process.Pid, sock), nil
	}); err != nil {
		t.Fatalf("a process holding the socket argument under another binary name was not matched: %v", err)
	}
}

func TestVerifyProcessCmdlineRejectsTheArgumentInsideAShellCommand(t *testing.T) {
	const sock = "/run/vm/api.sock"
	const script = "sleep 60; echo " + sock
	cmd := exec.Command("/bin/sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := WaitFor(t.Context(), 5*time.Second, time.Millisecond, func() (bool, error) {
		return VerifyProcessCmdline(cmd.Process.Pid, script), nil
	}); err != nil {
		t.Fatalf("child never execed: %v", err)
	}
	if VerifyProcessCmdline(cmd.Process.Pid, sock) {
		t.Fatal("a shell command that mentions the socket path was matched as the VMM")
	}
}

func TestVerifyForTerminationFailsClosed(t *testing.T) {
	injected := errors.New("injected cmdline read failure")
	failing := func(int, string) (bool, error) { return false, injected }
	matching := func(int, string) (bool, error) { return true, nil }

	tests := []struct {
		name    string
		pid     int
		verify  cmdlineVerifier
		alive   aliveFn
		want    bool
		wantErr error
	}{
		{"read error on live process fails closed", 42, failing, func(int) bool { return true }, false, injected},
		{"read error on dead process is a clean miss", 42, failing, func(int) bool { return false }, false, nil},
		{"clean match passes through", 42, matching, func(int) bool { return true }, true, nil},
		{"non-positive pid never matches", 0, matching, func(int) bool { return true }, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := verifyForTermination(tt.pid, "sock", tt.verify, tt.alive)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("got (%v, %v), want (%v, %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func fakeProc(files map[string]string) readFileFn {
	return func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, os.ErrNotExist
	}
}
