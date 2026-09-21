package cloudimg

import (
	"context"
	"errors"
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

var errBlobCacheMiss = errors.New("cached blob no longer exists")

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

	if sourcePath != "" && !utils.ValidFile(blobPath) {
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
	if !utils.ValidFile(blobPath) {
		path, err := ensureTmpBlob(ctx, conf, tracker, sourcePath, tmpBlobPath, digestHex)
		if err != nil {
			return err
		}
		tmpBlobPath = path
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

func ensureTmpBlob(ctx context.Context, conf *Config, tracker progress.Tracker, sourcePath, tmpBlobPath, digestHex string) (string, error) {
	if tmpBlobPath != "" {
		return tmpBlobPath, nil
	}
	if sourcePath == "" {
		return "", errBlobCacheMiss
	}
	return prepareTmpBlob(ctx, conf, tracker, sourcePath, digestHex)
}

func prepareTmpBlob(ctx context.Context, conf *Config, tracker progress.Tracker, sourcePath, digestHex string) (string, error) {
	logger := log.WithFunc("cloudimg.prepareTmpBlob")

	info, err := inspectImage(ctx, sourcePath)
	if err != nil {
		return "", fmt.Errorf("inspect image: %w", err)
	}
	logger.Debugf(ctx, "detected source format: %s (compat=%q, backing=%t)",
		info.Format, info.Compat, info.HasBackingFile)

	if info.Format == "qcow2" && info.Compat == "1.1" && !info.HasBackingFile {
		tmpBlobPath := conf.tmpBlobPath(digestHex)
		if err = os.Rename(sourcePath, tmpBlobPath); err != nil {
			return "", fmt.Errorf("rename tmp blob: %w", err)
		}
		logger.Debugf(ctx, "source already qcow2 v3, renamed to %s", tmpBlobPath)
		return tmpBlobPath, nil
	}

	lockPath := conf.tmpBlobPath(digestHex) + ".lock"
	convertLock := flock.New(lockPath)
	if err = convertLock.Lock(ctx); err != nil {
		return "", fmt.Errorf("acquire convert lock: %w", err)
	}
	defer convertLock.Unlock(ctx) //nolint:errcheck

	if utils.ValidFile(conf.BlobPath(digestHex)) {
		logger.Debugf(ctx, "blob %s committed while waiting for convert lock, skipping convert", digestHex[:12])
		return "", nil
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseConvert})
	// a process-unique target: two importers of one digest can both be past the lock while the other's convert is mid-write
	target, err := os.CreateTemp(conf.TempDir(), ".tmp-"+digestHex+"-*.qcow2")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpBlobPath := target.Name()
	_ = target.Close()
	if err := utils.RunQemuImg(ctx, "convert", "-f", info.Format, "-O", "qcow2", "-o", "compat=1.1", sourcePath, tmpBlobPath); err != nil {
		os.Remove(tmpBlobPath) //nolint:errcheck,gosec
		return "", err
	}
	logger.Debugf(ctx, "converted temp blob: %s", tmpBlobPath)
	return tmpBlobPath, nil
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
