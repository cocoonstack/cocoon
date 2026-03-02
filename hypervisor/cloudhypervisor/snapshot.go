package cloudhypervisor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

// Snapshot pauses the VM, captures its full state (CPU, memory, devices via CH
// snapshot API, plus the COW disk via sparse copy), resumes the VM, and returns
// a streaming tar.gz reader of the snapshot directory.
func (ch *CloudHypervisor) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error) {
	logger := log.WithFunc("cloudhypervisor.Snapshot")

	vmID, err := ch.resolveRef(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	rec, err := ch.loadRecord(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}

	sockPath := socketPath(rec.RunDir)

	// Determine COW file path and name inside the tar archive.
	var cowPath, cowName string
	if isDirectBoot(rec.BootConfig) {
		cowPath = ch.conf.COWRawPath(vmID)
		cowName = "cow.raw"
	} else {
		cowPath = ch.conf.OverlayPath(vmID)
		cowName = "overlay.qcow2"
	}

	// Create a temporary directory for the snapshot data.
	tmpDir, err := os.MkdirTemp(ch.conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}

	// withRunningVM verifies the process is alive, then runs the callback.
	// Inside the callback: pause → CH snapshot → SparseCopy COW → resume.
	if err := ch.withRunningVM(&rec, func(_ int) error {
		if err := pauseVM(ctx, sockPath); err != nil {
			return fmt.Errorf("pause: %w", err)
		}

		resumed := false
		doResume := func() {
			if resumed {
				return
			}
			resumed = true
			if resumeErr := resumeVM(context.WithoutCancel(ctx), sockPath); resumeErr != nil {
				logger.Warnf(ctx, "resume VM %s: %v", vmID, resumeErr)
			}
		}
		defer doResume()

		if err := snapshotVM(ctx, sockPath, tmpDir); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}

		if err := utils.SparseCopy(filepath.Join(tmpDir, cowName), cowPath); err != nil {
			return fmt.Errorf("sparse copy COW: %w", err)
		}
		return nil
	}); err != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}

	// Build SnapshotConfig from the VM record.
	cfg := &types.SnapshotConfig{}
	if rec.ImageBlobIDs != nil {
		cfg.ImageBlobIDs = make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(cfg.ImageBlobIDs, rec.ImageBlobIDs)
	}

	// Stream tmpDir as tar.gz via io.Pipe. Goroutine cleans up tmpDir when done.
	pr, pw := io.Pipe()
	go func() {
		defer os.RemoveAll(tmpDir) //nolint:errcheck
		var streamErr error
		defer func() {
			if streamErr != nil {
				pw.CloseWithError(streamErr) //nolint:errcheck,gosec
			} else {
				pw.Close() //nolint:errcheck,gosec
			}
		}()

		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)

		streamErr = utils.TarDir(tw, tmpDir)
		if closeErr := tw.Close(); streamErr == nil {
			streamErr = closeErr
		}
		if closeErr := gw.Close(); streamErr == nil {
			streamErr = closeErr
		}
	}()

	return cfg, pr, nil
}
