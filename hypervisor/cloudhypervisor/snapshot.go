package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// snapshotMetaFile is the cocoon-owned sidecar inside a CH snapshot.
// It mirrors snapshot config.json's disk shape but carries Role/MountPoint/
// FSType/DirectIO that the CH schema cannot.
const snapshotMetaFile = "cocoon.json"

// snapshotMeta is persisted as cocoon.json alongside CH state.json/config.json.
// StorageConfigs is in the same order and length as snapshot config.json's disks.
type snapshotMeta struct {
	StorageConfigs []*types.StorageConfig `json:"storage_configs"`
	BootConfig     *types.BootConfig      `json:"boot_config,omitempty"`
}

// Snapshot pauses the VM, captures its full state (CPU, memory, devices via CH
// snapshot API, plus the COW disk via sparse copy), resumes the VM, and returns
// a streaming tar.gz reader of the snapshot directory.
func (ch *CloudHypervisor) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error) {
	logger := log.WithFunc("cloudhypervisor.Snapshot")

	vmID, err := ch.ResolveRef(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	rec, err := ch.LoadRecord(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	hc := utils.NewSocketHTTPClient(sockPath)

	// Determine COW file path and name inside the tar archive.
	directBoot := isDirectBoot(rec.BootConfig)
	cowPath := ch.cowPath(vmID, directBoot)
	cowName := "overlay.qcow2"
	if directBoot {
		cowName = "cow.raw"
	}

	// Create a temporary directory for the snapshot data.
	tmpDir, err := os.MkdirTemp(ch.conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}

	// WithRunningVM verifies the process is alive, then runs the callback.
	// Inside the callback: pause → CH snapshot → SparseCopy COW → resume.
	if err := ch.WithRunningVM(ctx, &rec, func(_ int) error {
		if err := pauseVM(ctx, hc); err != nil {
			return fmt.Errorf("pause: %w", err)
		}

		resumed := false
		var resumeErr error
		doResume := func() {
			if resumed {
				return
			}
			resumed = true
			resumeErr = resumeVM(context.WithoutCancel(ctx), hc)
			if resumeErr != nil {
				logger.Warnf(ctx, "resume VM %s: %v", vmID, resumeErr)
			}
		}
		defer doResume()

		if err := snapshotVM(ctx, hc, tmpDir); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}

		if err := utils.ReflinkCopy(filepath.Join(tmpDir, cowName), cowPath); err != nil {
			return fmt.Errorf("copy COW: %w", err)
		}

		for _, sc := range rec.StorageConfigs {
			if sc.Role != types.StorageRoleData {
				continue
			}
			dst := filepath.Join(tmpDir, "data-"+sc.Serial+".raw")
			if err := utils.ReflinkCopy(dst, sc.Path); err != nil {
				return fmt.Errorf("copy data disk %s: %w", sc.Serial, err)
			}
		}

		// Resume eagerly so we can propagate the error.
		// The deferred doResume is a no-op when resumed=true.
		doResume()
		if resumeErr != nil {
			return fmt.Errorf("snapshot data captured but resume failed: %w", resumeErr)
		}
		return nil
	}); err != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}

	// For cloudimg VMs, include cidata.img (per-VM cloud-init disk).
	// cidata is read-only and static, so it can be copied outside the pause window.
	if !directBoot && !rec.Config.Windows {
		cidataSrc := ch.conf.CidataPath(vmID)
		if _, statErr := os.Stat(cidataSrc); statErr == nil {
			if cpErr := utils.SparseCopy(filepath.Join(tmpDir, cidataFile), cidataSrc); cpErr != nil {
				os.RemoveAll(tmpDir) //nolint:errcheck,gosec
				return nil, nil, fmt.Errorf("copy cidata: %w", cpErr)
			}
		}
	}

	if metaErr := writeSnapshotMeta(tmpDir, &rec); metaErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("write snapshot metadata: %w", metaErr)
	}

	// Generate snapshot ID and record it on the VM atomically.
	snapID, recErr := ch.RecordSnapshot(ctx, vmID, tmpDir)
	if recErr != nil {
		return nil, nil, recErr
	}

	return ch.BuildSnapshotConfig(snapID, &rec), utils.TarDirStreamWithRemove(tmpDir), nil
}

// writeSnapshotMeta builds the cocoon.json sidecar. Sidecar mirrors the
// snapshot's own config.json shape (chCfg.Disks order/length), not
// activeDisks(rec) — the latter would diverge whenever a cloudimg VM is
// snapshotted between FirstBooted=true and the next stop, since CH still
// holds cidata in that window.
func writeSnapshotMeta(tmpDir string, rec *hypervisor.VMRecord) error {
	chCfg, _, err := parseCHConfig(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		return fmt.Errorf("parse snapshot config: %w", err)
	}
	byPath := make(map[string]*types.StorageConfig, len(rec.StorageConfigs))
	for _, sc := range rec.StorageConfigs {
		byPath[sc.Path] = sc
	}
	storage := make([]*types.StorageConfig, 0, len(chCfg.Disks))
	for _, d := range chCfg.Disks {
		sc, ok := byPath[d.Path]
		if !ok {
			return fmt.Errorf("snapshot config has disk %q not present in VM record", d.Path)
		}
		cp := *sc
		storage = append(storage, &cp)
	}
	meta := snapshotMeta{
		StorageConfigs: storage,
		BootConfig:     rec.BootConfig,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal snapshot meta: %w", err)
	}
	return os.WriteFile(filepath.Join(tmpDir, snapshotMetaFile), data, 0o600)
}

// loadSnapshotMeta reads cocoon.json from a CH snapshot directory.
func loadSnapshotMeta(dir string) (*snapshotMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotMetaFile)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", snapshotMetaFile, err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode %s: %w", snapshotMetaFile, err)
	}
	return &meta, nil
}
