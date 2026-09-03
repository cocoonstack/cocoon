package firecracker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	snapshotVMStateFile = "vmstate"
	snapshotMemFile     = "mem"
)

func (fc *Firecracker) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, string, error) {
	return fc.SnapshotSequence(ctx, ref, fc.snapshotSpec(ctx))
}

func (fc *Firecracker) Hibernate(ctx context.Context, ref string, persist func(cfg *types.SnapshotConfig, srcDir string) error) error {
	return fc.HibernateSequence(ctx, ref, hypervisor.HibernateSpec{
		SnapshotSpec: fc.snapshotSpec(ctx),
		Terminate: func(rec *hypervisor.VMRecord, _ *http.Client, pid int) error {
			return fc.terminateVMM(ctx, rec, pid)
		},
		RuntimeFiles: runtimeFiles,
	}, persist)
}

func (fc *Firecracker) snapshotSpec(ctx context.Context) hypervisor.SnapshotSpec {
	return hypervisor.SnapshotSpec{
		Pause:  func(_ *hypervisor.VMRecord, hc *http.Client) error { return pauseVM(ctx, hc) },
		Resume: func(_ *hypervisor.VMRecord, hc *http.Client) error { return resumeVM(context.WithoutCancel(ctx), hc) },
		// createSnapshotFC builds its own client with VMMemTransferTimeout (multi-GiB memory transfer); it cannot share hc.
		Capture: func(rec *hypervisor.VMRecord, _ *http.Client, tmpDir string) error {
			sockPath := hypervisor.SocketPath(rec.RunDir)
			if err := createSnapshotFC(ctx, sockPath, tmpDir); err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			cowPath := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW)
			if cowPath == "" {
				return fmt.Errorf("no COW disk recorded for %s", rec.ID)
			}
			return hypervisor.CopyWritableDisks(ctx, tmpDir, cowPath, rec.StorageConfigs)
		},
		BuildMeta: buildSnapshotMeta,
	}
}

// buildSnapshotMeta rewrites kernel path to vmlinuz so clones get the portable artifact instead of the FC-specific vmlinux cache.
func buildSnapshotMeta(rec *hypervisor.VMRecord, _ string) (*hypervisor.SnapshotMeta, error) {
	meta := &hypervisor.SnapshotMeta{
		StorageConfigs: hypervisor.CloneStorageConfigs(rec.StorageConfigs),
	}
	if rec.BootConfig != nil {
		b := *rec.BootConfig
		if filepath.Base(b.KernelPath) == "vmlinux" {
			b.KernelPath = filepath.Join(filepath.Dir(b.KernelPath), "vmlinuz")
		}
		b.Cmdline = "" // cmdline is rebuilt on clone with new VM name/IP
		meta.BootConfig = &b
	}
	return meta, nil
}
