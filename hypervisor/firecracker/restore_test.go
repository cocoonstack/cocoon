package firecracker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestPreflightRestoreRefusesAnotherVMsDrives(t *testing.T) {
	fc := newTestFC(t)
	srcDir := t.TempDir()
	for _, name := range []string{types.COWRawFileName, snapshotVMStateFile, snapshotMemFile} {
		writeTestFile(t, srcDir, name)
	}
	sourceCOW := fc.conf.COWRawPath("vm-a")
	if err := hypervisor.SaveSnapshotMeta(srcDir, &hypervisor.SnapshotMeta{
		StorageConfigs: []*types.StorageConfig{{Role: types.StorageRoleCOW, Serial: hypervisor.CowSerial, Path: sourceCOW}},
	}); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	recordFor := func(cowPath string) *hypervisor.VMRecord {
		return &hypervisor.VMRecord{
			ID:             filepath.Base(filepath.Dir(cowPath)),
			StorageConfigs: []*types.StorageConfig{{Role: types.StorageRoleCOW, Serial: hypervisor.CowSerial, Path: cowPath}},
		}
	}

	err := fc.preflightRestore(srcDir, recordFor(fc.conf.COWRawPath("vm-b")))
	if err == nil || !strings.Contains(err.Error(), "is recorded at "+sourceCOW) {
		t.Fatalf("err = %v, want a refusal naming %s", err, sourceCOW)
	}
	if err := fc.preflightRestore(srcDir, recordFor(sourceCOW)); err != nil {
		t.Fatalf("same-VM preflight: %v", err)
	}
}
