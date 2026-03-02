package cloudhypervisor

import (
	"archive/tar"
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

// snapshotReader wraps io.PipeReader so that Close waits for the background
// goroutine to finish cleanup (removing tmpDir) before returning.
// streamErr captures the goroutine's error so Close can surface it even
// after all data has been read (PipeReader.Close always returns nil).
type snapshotReader struct {
	*io.PipeReader
	done      <-chan struct{}
	streamErr *error
}

func (r *snapshotReader) Close() error {
	err := r.PipeReader.Close()
	<-r.done
	if *r.streamErr != nil {
		return *r.streamErr
	}
	return err
}

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
	hc := utils.NewSocketHTTPClient(sockPath)

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
		if err := pauseVM(ctx, hc); err != nil {
			return fmt.Errorf("pause: %w", err)
		}

		resumed := false
		doResume := func() {
			if resumed {
				return
			}
			resumed = true
			if resumeErr := resumeVM(context.WithoutCancel(ctx), hc); resumeErr != nil {
				logger.Warnf(ctx, "resume VM %s: %v", vmID, resumeErr)
			}
		}
		defer doResume()

		if err := snapshotVM(ctx, hc, tmpDir); err != nil {
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

	// For cloudimg VMs, include cidata.img (per-VM cloud-init disk).
	// cidata is read-only and static, so it can be copied outside the pause window.
	if !isDirectBoot(rec.BootConfig) {
		cidataSrc := ch.conf.CidataPath(vmID)
		if _, statErr := os.Stat(cidataSrc); statErr == nil {
			if cpErr := utils.SparseCopy(filepath.Join(tmpDir, "cidata.img"), cidataSrc); cpErr != nil {
				os.RemoveAll(tmpDir) //nolint:errcheck,gosec
				return nil, nil, fmt.Errorf("copy cidata: %w", cpErr)
			}
		}
	}

	// Build SnapshotConfig from the VM record.
	cfg := &types.SnapshotConfig{}
	if rec.ImageBlobIDs != nil {
		cfg.ImageBlobIDs = make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(cfg.ImageBlobIDs, rec.ImageBlobIDs)
	}

	// Stream tmpDir as tar via io.Pipe. Goroutine cleans up tmpDir when done.
	// snapshotReader.Close waits for the goroutine to finish so tmpDir is
	// always removed before the process exits.
	pr, pw := io.Pipe()
	done := make(chan struct{})
	var streamErr error
	go func() {
		defer close(done)
		defer os.RemoveAll(tmpDir) //nolint:errcheck
		defer func() {
			if streamErr != nil {
				pw.CloseWithError(streamErr) //nolint:errcheck,gosec
			} else {
				pw.Close() //nolint:errcheck,gosec
			}
		}()

		tw := tar.NewWriter(pw)

		streamErr = utils.TarDir(tw, tmpDir)
		if closeErr := tw.Close(); streamErr == nil {
			streamErr = closeErr
		}
	}()

	return cfg, &snapshotReader{PipeReader: pr, done: done, streamErr: &streamErr}, nil
}
