package cloudhypervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestExportCloudImageFlattensStoppedVM(t *testing.T) {
	binDir := t.TempDir()
	qemuImg := filepath.Join(binDir, "qemu-img")
	if err := os.WriteFile(qemuImg, []byte("#!/bin/sh\nfor last in \"$@\"; do :; done\nprintf flattened > \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ch := newTestCH(t)
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	if err := os.WriteFile(overlay, []byte("overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedExportVM(t, ch, "vm1", "alpha", types.ImageTypeCloudImg, overlay)
	dest := filepath.Join(t.TempDir(), "export.qcow2")
	vm, err := ch.ExportCloudImage(t.Context(), "alpha", dest)
	if err != nil {
		t.Fatal(err)
	}
	if vm.ID != "vm1" {
		t.Fatalf("exported VM ID = %q", vm.ID)
	}
	got, err := os.ReadFile(dest) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "flattened" {
		t.Fatalf("export content = %q", got)
	}
	rec, err := ch.LoadRecord(t.Context(), "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != types.VMStateStopped {
		t.Fatalf("state = %s, want stopped", rec.State)
	}
}

func TestExportCloudImageRejectsOCIBackedVM(t *testing.T) {
	ch := newTestCH(t)
	seedExportVM(t, ch, "vm1", "alpha", types.ImageTypeOCI, "/tmp/cow.raw")
	if _, err := ch.ExportCloudImage(t.Context(), "alpha", filepath.Join(t.TempDir(), "export.qcow2")); err == nil {
		t.Fatal("expected OCI-backed VM export to fail")
	}
}

func TestExportCloudImageDoesNotRestartStaleRunningRecord(t *testing.T) {
	binDir := t.TempDir()
	qemuImg := filepath.Join(binDir, "qemu-img")
	if err := os.WriteFile(qemuImg, []byte("#!/bin/sh\nfor last in \"$@\"; do :; done\nprintf flattened > \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ch := newTestCH(t)
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	if err := os.WriteFile(overlay, []byte("overlay"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedExportVM(t, ch, "vm1", "alpha", types.ImageTypeCloudImg, overlay)
	if err := ch.UpdateRecord(t.Context(), "vm1", func(r *hypervisor.VMRecord) error {
		r.State = types.VMStateRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ch.ExportCloudImage(t.Context(), "alpha", filepath.Join(t.TempDir(), "export.qcow2")); err != nil {
		t.Fatalf("export stale-running record: %v", err)
	}
	rec, err := ch.LoadRecord(t.Context(), "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != types.VMStateStopped {
		t.Fatalf("state = %s, want stopped without relaunch", rec.State)
	}
}

func seedExportVM(t *testing.T, ch *CloudHypervisor, id, name, imageType, cowPath string) {
	t.Helper()
	cfg := &types.VMConfig{Name: name, Config: types.Config{ImageType: imageType}}
	if err := ch.PrereserveVM(t.Context(), id, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := ch.UpdateRecord(t.Context(), id, func(r *hypervisor.VMRecord) error {
		r.State = types.VMStateStopped
		r.StorageConfigs = []*types.StorageConfig{{Path: cowPath, Role: types.StorageRoleCOW}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
