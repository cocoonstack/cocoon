package cloudimg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

func commit(
	ctx context.Context,
	conf *Config,
	store storage.Store[imageIndex],
	ref string,
	tracker progress.Tracker,
	sourcePath string,
	digestHex string,
) error {
	logger := log.WithFunc("cloudimg.commit")

	blobPath := conf.BlobPath(digestHex)
	var tmpBlobPath string

	// Best-effort cleanup if commit aborts before the final rename.
	defer func() {
		if tmpBlobPath != "" {
			os.Remove(tmpBlobPath) //nolint:errcheck,gosec
		}
	}()

	if !utils.ValidFile(blobPath) {
		path, err := prepareTmpBlob(ctx, conf, tracker, sourcePath, digestHex)
		if err != nil {
			return err
		}
		tmpBlobPath = path
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseCommit})

	if err := store.Update(ctx, func(idx *imageIndex) error {
		if tmpBlobPath != "" && !utils.ValidFile(blobPath) {
			if renameErr := os.Rename(tmpBlobPath, blobPath); renameErr != nil {
				return fmt.Errorf("rename blob: %w", renameErr)
			}
			if chmodErr := os.Chmod(blobPath, 0o444); chmodErr != nil { //nolint:gosec // G302: intentionally world-readable
				logger.Warnf(ctx, "chmod blob %s: %v", blobPath, chmodErr)
			}
		}
		return writeIndexEntry(idx, conf, ref, digestHex)
	}); err != nil {
		return fmt.Errorf("update index: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDone})
	return nil
}

func commitExistingBlob(
	ctx context.Context,
	conf *Config,
	store storage.Store[imageIndex],
	ref string,
	digestHex string,
	tracker progress.Tracker,
) error {
	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseCommit})

	if err := store.Update(ctx, func(idx *imageIndex) error {
		return writeIndexEntry(idx, conf, ref, digestHex)
	}); err != nil {
		return fmt.Errorf("update index: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDone})
	return nil
}

func prepareTmpBlob(
	ctx context.Context,
	conf *Config,
	tracker progress.Tracker,
	sourcePath string,
	digestHex string,
) (string, error) {
	logger := log.WithFunc("cloudimg.commit")

	info, err := inspectImage(ctx, sourcePath)
	if err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	logger.Debugf(ctx, "detected source format: %s (compat=%q, backing=%t)",
		info.Format, info.Compat, info.HasBackingFile)

	if info.Format == "qcow2" && info.Compat == "1.1" && !info.HasBackingFile {
		// Fast path: the source is already a final-form qcow2 blob.
		tmpBlobPath := conf.tmpBlobPath(digestHex)
		if err := os.Rename(sourcePath, tmpBlobPath); err != nil {
			return "", fmt.Errorf("rename tmp blob: %w", err)
		}
		logger.Debugf(ctx, "source already qcow2 v3, renamed to %s", tmpBlobPath)
		return tmpBlobPath, nil
	}

	// Slow path: serialize per-digest conversion work.
	lockPath := conf.tmpBlobPath(digestHex) + ".lock"
	convertLock := flock.New(lockPath)
	if err := convertLock.Lock(ctx); err != nil {
		return "", fmt.Errorf("acquire convert lock: %w", err)
	}
	defer convertLock.Unlock(ctx) //nolint:errcheck

	if utils.ValidFile(conf.BlobPath(digestHex)) {
		logger.Debugf(ctx, "blob %s committed while waiting for convert lock, skipping convert", digestHex[:12])
		return "", nil
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseConvert})
	tmpBlobPath := conf.tmpBlobPath(digestHex)
	if err := convertToQcow2(ctx, info.Format, sourcePath, tmpBlobPath); err != nil {
		return "", err
	}
	logger.Debugf(ctx, "converted temp blob: %s", tmpBlobPath)
	return tmpBlobPath, nil
}

func convertToQcow2(ctx context.Context, srcFormat, src, dst string) error {
	// shell out because no Go qcow2 writer; qemu-img is authoritative for format conversion.
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", //nolint:gosec // args are controlled
		"-f", srcFormat, "-O", "qcow2", "-o", "compat=1.1",
		src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(dst) //nolint:errcheck,gosec
		return fmt.Errorf("qemu-img convert: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func writeIndexEntry(idx *imageIndex, conf *Config, ref, digestHex string) error {
	blobPath := conf.BlobPath(digestHex)
	info, err := os.Stat(blobPath)
	if err != nil {
		return fmt.Errorf("stat blob %s: %w", blobPath, err)
	}
	idx.Images[ref] = &imageEntry{
		Ref:        ref,
		ContentSum: images.NewDigest(digestHex),
		Size:       info.Size(),
		CreatedAt:  time.Now(),
	}
	return nil
}
