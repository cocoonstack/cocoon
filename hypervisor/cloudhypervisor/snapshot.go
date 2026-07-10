package cloudhypervisor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Snapshot pauses, captures CH state+COW, resumes, and returns the capture dir.
func (ch *CloudHypervisor) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, string, error) {
	return ch.SnapshotSequence(ctx, ref, ch.snapshotSpec(ctx))
}

// Hibernate captures like Snapshot but persists and then terminates the VMM inside the pause window instead of resuming.
func (ch *CloudHypervisor) Hibernate(ctx context.Context, ref string, persist func(cfg *types.SnapshotConfig, srcDir string) error) error {
	return ch.HibernateSequence(ctx, ref, hypervisor.HibernateSpec{
		SnapshotSpec: ch.snapshotSpec(ctx),
		Terminate: func(rec *hypervisor.VMRecord, hc *http.Client, pid int) error {
			return ch.terminateVMM(ctx, rec, hc, pid)
		},
		RuntimeFiles: runtimeFiles,
	}, persist)
}

func (ch *CloudHypervisor) snapshotSpec(ctx context.Context) hypervisor.SnapshotSpec {
	return hypervisor.SnapshotSpec{
		Pause:  func(_ *hypervisor.VMRecord, hc *http.Client) error { return pauseVM(ctx, hc) },
		Resume: func(_ *hypervisor.VMRecord, hc *http.Client) error { return resumeVM(context.WithoutCancel(ctx), hc) },
		Capture: func(rec *hypervisor.VMRecord, hc *http.Client, tmpDir string) error {
			if err := snapshotVM(ctx, hc, tmpDir); err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			// Recorded path, not conf-derived: after a --run-dir change the disks stay where the record says.
			cowPath := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW)
			if cowPath == "" {
				return fmt.Errorf("no COW disk recorded for %s", rec.ID)
			}
			return hypervisor.CopyWritableDisks(ctx, tmpDir, cowPath, rec.StorageConfigs)
		},
		AfterCapture: func(rec *hypervisor.VMRecord, tmpDir string) error {
			if hypervisor.IsDirectBoot(rec.BootConfig) || rec.Config.Windows {
				return nil
			}
			cidataSrc := hypervisor.DiskPathByRole(rec.StorageConfigs, types.StorageRoleCidata)
			if cidataSrc == "" { // pre-first-boot: file exists but is not yet a recorded disk
				cidataSrc = filepath.Join(rec.RunDir, cidataFile)
			}
			if _, statErr := os.Stat(cidataSrc); statErr != nil {
				return nil
			}
			if cpErr := utils.SparseCopy(filepath.Join(tmpDir, cidataFile), cidataSrc); cpErr != nil {
				return fmt.Errorf("copy cidata: %w", cpErr)
			}
			return nil
		},
		BuildMeta: buildSnapshotMeta,
	}
}

// buildSnapshotMeta mirrors config.json's disk shape (not activeDisks — diverges for cloudimg post-FirstBooted pre-restart where CH still holds cidata).
func buildSnapshotMeta(rec *hypervisor.VMRecord, tmpDir string) (*hypervisor.SnapshotMeta, error) {
	chCfg, err := parseCHConfig(filepath.Join(tmpDir, configJSONName))
	if err != nil {
		return nil, fmt.Errorf("parse snapshot config: %w", err)
	}
	byPath := make(map[string]*types.StorageConfig, len(rec.StorageConfigs))
	for _, sc := range rec.StorageConfigs {
		byPath[sc.Path] = sc
	}
	ordered := make([]*types.StorageConfig, 0, len(chCfg.Disks))
	for _, d := range chCfg.Disks {
		sc, ok := byPath[d.Path]
		if !ok {
			if name := disk.NameFromID(d.ID); name != "" {
				return nil, fmt.Errorf("hot-attached disk %q: detach before snapshot or hibernate", name)
			}
			return nil, fmt.Errorf("snapshot config has disk %q not present in VM record", d.Path)
		}
		ordered = append(ordered, sc)
	}
	return &hypervisor.SnapshotMeta{
		StorageConfigs: hypervisor.CloneStorageConfigs(ordered),
		BootConfig:     rec.BootConfig,
	}, nil
}
