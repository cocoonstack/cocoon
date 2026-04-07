package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

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

	// Generate snapshot ID and record it on the VM atomically.
	snapID, genErr := utils.GenerateID()
	if genErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("generate snapshot ID: %w", genErr)
	}
	if updateErr := ch.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("vm %s disappeared from index", vmID)
		}
		if r.SnapshotIDs == nil {
			r.SnapshotIDs = make(map[string]struct{})
		}
		r.SnapshotIDs[snapID] = struct{}{}
		return nil
	}); updateErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("record snapshot on VM: %w", updateErr)
	}

	// Build SnapshotConfig from the VM record.
	cfg := &types.SnapshotConfig{
		ID:         snapID,
		Image:      rec.Config.Image,
		Hypervisor: typ,
		CPU:        rec.Config.CPU,
		Memory:     rec.Config.Memory,
		Storage:    rec.Config.Storage,
		NICs:       len(rec.NetworkConfigs),
		Network:    rec.Config.Network,
		Windows:    rec.Config.Windows,
	}
	if rec.ImageBlobIDs != nil {
		cfg.ImageBlobIDs = make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(cfg.ImageBlobIDs, rec.ImageBlobIDs)
	}

	return cfg, utils.TarDirStreamWithRemove(tmpDir), nil
}
