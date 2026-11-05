package firecracker

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
)

const (
	snapshotVMStateFile = "vmstate"
	snapshotMemFile     = "mem"
)

func (fc *Firecracker) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, string, error) {
	return fc.SnapshotSequence(ctx, ref, fc.snapshotSpec(ctx))
}

func (fc *Firecracker) Hibernate(ctx context.Context, ref string, persist hypervisor.PersistFunc) error {
	return fc.HibernateSequence(ctx, ref, hypervisor.HibernateSpec{
		SnapshotSpec: fc.snapshotSpec(ctx),
		Terminate: func(rec *hypervisor.VMRecord, pid int) error {
			return fc.terminateVMM(ctx, rec, pid)
		},
		RuntimeFiles: runtimeFiles,
	}, persist)
}

func (fc *Firecracker) snapshotSpec(ctx context.Context) hypervisor.SnapshotSpec {
	return hypervisor.SnapshotSpec{
		Pause: func(rec *hypervisor.VMRecord, hc *http.Client) error {
			if rec.Config.PCI {
				if err := refuseUnrecordedDevices(ctx, hc, rec); err != nil {
					return err
				}
			}
			return pauseVM(ctx, hc)
		},
		Resume: func(_ *hypervisor.VMRecord, hc *http.Client) error { return resumeVM(context.WithoutCancel(ctx), hc) },
		// createSnapshotFC builds its own client with VMMemTransferTimeout (multi-GiB memory transfer); it cannot share hc.
		Capture: func(rec *hypervisor.VMRecord, _ *http.Client, tmpDir string) error {
			sockPath := hypervisor.SocketPath(rec.RunDir)
			if err := createSnapshotFC(ctx, sockPath, tmpDir); err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			return hypervisor.CopyWritableDisks(ctx, tmpDir, rec)
		},
		BuildMeta: buildSnapshotMeta,
	}
}

// buildSnapshotMeta rewrites kernel path to vmlinuz so clones get the portable artifact instead of the FC-specific vmlinux cache.
func buildSnapshotMeta(rec *hypervisor.VMRecord, _ string) (*snapshot.SnapshotMeta, error) {
	macs := make([]string, len(rec.NetworkConfigs))
	for i, nc := range rec.NetworkConfigs {
		macs[i] = nc.MAC
	}
	meta := &snapshot.SnapshotMeta{
		StorageConfigs: hypervisor.CloneStorageConfigs(rec.StorageConfigs),
		NICMACs:        macs,
	}
	if rec.BootConfig != nil {
		b := *rec.BootConfig
		if filepath.Base(b.KernelPath) == "vmlinux" {
			b.KernelPath = filepath.Join(filepath.Dir(b.KernelPath), "vmlinuz")
		}
		meta.BootConfig = &b
	}
	return meta, nil
}

// A runtime-only disk or NIC has no record entry, so the snapshot could never restore.
func refuseUnrecordedDevices(ctx context.Context, hc *http.Client, rec *hypervisor.VMRecord) error {
	cfg, err := getVMConfig(ctx, hc)
	if err != nil {
		return err
	}
	if hot := hotAttachedDisks(cfg); len(hot) > 0 {
		return fmt.Errorf("hot-attached disk %q: %w", hot[0].Name, hypervisor.ErrHotAttached)
	}
	if len(cfg.NetworkInterfaces) != len(rec.NetworkConfigs) {
		return fmt.Errorf("vm %s has %d NICs but its record has %d; rerun vm net first", rec.ID, len(cfg.NetworkInterfaces), len(rec.NetworkConfigs))
	}
	return nil
}
