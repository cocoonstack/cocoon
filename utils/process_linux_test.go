//go:build linux

package utils

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
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
		alive   func(int) bool
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
