package firecracker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
)

func TestPreflightRestoreRefusesAnotherVMsDrives(t *testing.T) {
	fc := newTestFC(t)
	srcDir := t.TempDir()
	for _, name := range []string{types.COWRawFileName, snapshotVMStateFile, snapshotMemFile} {
		writeTestFile(t, srcDir, name)
	}
	sourceCOW := fc.conf.COWRawPath("vm-a")
	if err := snapshot.SaveSnapshotMeta(srcDir, &snapshot.SnapshotMeta{
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

func TestPreflightRestoreRefusesDriftedNICs(t *testing.T) {
	const snapMAC = "02:00:00:00:00:01"
	fc := newTestFC(t)
	srcDir := t.TempDir()
	for _, name := range []string{types.COWRawFileName, snapshotVMStateFile, snapshotMemFile} {
		writeTestFile(t, srcDir, name)
	}
	cowPath := fc.conf.COWRawPath("vm-a")
	recordWith := func(macs ...string) *hypervisor.VMRecord {
		rec := &hypervisor.VMRecord{
			ID:             "vm-a",
			StorageConfigs: []*types.StorageConfig{{Role: types.StorageRoleCOW, Serial: hypervisor.CowSerial, Path: cowPath}},
		}
		for _, mac := range macs {
			rec.NetworkConfigs = append(rec.NetworkConfigs, &types.NetworkConfig{MAC: mac})
		}
		return rec
	}
	meta, err := buildSnapshotMeta(recordWith(snapMAC), "")
	if err != nil {
		t.Fatalf("build sidecar: %v", err)
	}
	if err := snapshot.SaveSnapshotMeta(srcDir, meta); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	for _, tt := range []struct {
		name    string
		rec     *hypervisor.VMRecord
		wantErr string
	}{
		{name: "same MAC", rec: recordWith(snapMAC)},
		{name: "MAC case differs", rec: recordWith(strings.ToUpper(snapMAC))},
		{name: "drifted MAC", rec: recordWith("02:00:00:00:00:02"), wantErr: "nic 0 MAC drifted"},
		{name: "NIC removed", rec: recordWith(), wantErr: "NIC identity must match"},
		{name: "NIC added after the snapshot", rec: recordWith(snapMAC, "02:00:00:00:00:03"), wantErr: "NIC identity must match"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := fc.preflightRestore(srcDir, tt.rec)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("preflight: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}

	meta.NICMACs = nil
	if err := snapshot.SaveSnapshotMeta(srcDir, meta); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if err := fc.preflightRestore(srcDir, recordWith("02:00:00:00:00:02")); err != nil {
		t.Fatalf("preflight with a sidecar without MACs: %v", err)
	}
}
