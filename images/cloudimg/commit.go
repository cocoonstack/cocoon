package cloudimg

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/utils"
)

func commit(ctx context.Context, conf *Config, store *images.Store[imageEntry], ref string, tracker progress.Tracker, sourcePath, digestHex string) error {
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

	// Lock spans rename→index-commit so GC cannot delete a blob that is on disk but not yet indexed (design §5).
	var blobLocks images.BlobLocks
	defer blobLocks.Release()
	if err := blobLocks.Lock(conf.BlobLockPath(digestHex)); err != nil {
		return err
	}
	if tmpBlobPath != "" && !utils.ValidFile(blobPath) {
		if renameErr := os.Rename(tmpBlobPath, blobPath); renameErr != nil {
			return fmt.Errorf("rename blob: %w", renameErr)
		}
		if chmodErr := os.Chmod(blobPath, 0o444); chmodErr != nil { //nolint:gosec // G302: intentionally world-readable
			logger.Warnf(ctx, "chmod blob %s: %v", blobPath, chmodErr)
		}
	}
	if err := store.Update(ctx, func(idx *imageIndex) error {
		return writeIndexEntry(idx, conf, ref, digestHex)
	}); err != nil {
		return fmt.Errorf("update index: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDone})
	return nil
}

func prepareTmpBlob(ctx context.Context, conf *Config, tracker progress.Tracker, sourcePath, digestHex string) (string, error) {
	logger := log.WithFunc("cloudimg.prepareTmpBlob")

	info, err := inspectImage(ctx, sourcePath)
	if err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	logger.Debugf(ctx, "detected source format: %s (compat=%q, backing=%t)",
		info.Format, info.Compat, info.HasBackingFile)

	if info.Format == qcow2Format && info.Compat == "1.1" && !info.HasBackingFile {
		tmpBlobPath := conf.tmpBlobPath(digestHex)
		if err := os.Rename(sourcePath, tmpBlobPath); err != nil {
			return "", fmt.Errorf("rename tmp blob: %w", err)
		}
		logger.Debugf(ctx, "source already qcow2 v3, renamed to %s", tmpBlobPath)
		return tmpBlobPath, nil
	}

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
	if err := utils.RunQemuImg(ctx, "convert", "-f", srcFormat, "-O", qcow2Format, "-o", "compat=1.1", src, dst); err != nil {
		os.Remove(dst) //nolint:errcheck,gosec
		return err
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
