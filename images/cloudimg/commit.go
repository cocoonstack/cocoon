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

// commit validates a pre-sniffed disk image source, converts it to
// qcow2 v3 if necessary, and atomically places the result as a blob at
// conf.BlobPath(digestHex) while updating the index under ref.
//
// sourcePath MUST be a cocoon-owned file that commit is free to rename
// or consume — typically a download temp file (pull) or an import temp
// file (importQcow2*). User-owned files must be copied to a cocoon temp
// by the caller before entering commit; see importQcow2File.
//
// IMPORTANT: commit does NOT sniff sourcePath for non-disk-image
// content. Callers MUST call sniffImageSource (or an equivalent check)
// first. This is a deliberate design choice so that callers holding an
// open download handle can validate via ReadAt without reopening.
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

	// Clean up the intermediate tmp blob on abort. Once the commit-phase
	// store.Update renames tmpBlobPath into blobPath, the path is gone
	// and os.Remove becomes a silent no-op.
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

// commitExistingBlob registers ref → digestHex in the index for a blob
// that is already present at conf.BlobPath(digestHex). Use this from a
// caller that has already computed the digest and verified the blob is
// cached (via utils.ValidFile) — notably importQcow2File skipping its
// clone pass when the user's file has been imported before under a
// different name.
//
// If the blob was removed between the caller's ValidFile check and
// this call (e.g., concurrent GC), the inner os.Stat surfaces a
// "stat blob: no such file" error and the caller can retry with the
// full import pipeline.
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

// prepareTmpBlob inspects sourcePath and produces an intermediate qcow2
// temp blob in conf.TempDir() ready for the commit-phase rename into
// blobPath. Returns an empty path with nil error when another process
// committed the blob while we waited on the slow-path convert lock —
// commit will then skip the rename and just write the index entry.
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
		// Fast path: rename the already-qcow2-v3 source into the
		// digest-derived tmp slot. Collisions with concurrent
		// pulls/imports of identical content are benign — content is
		// identical and the commit-phase flock serializes the final
		// rename into blobPath.
		tmpBlobPath := conf.tmpBlobPath(digestHex)
		if err := os.Rename(sourcePath, tmpBlobPath); err != nil {
			return "", fmt.Errorf("rename tmp blob: %w", err)
		}
		logger.Debugf(ctx, "source already qcow2 v3, renamed to %s", tmpBlobPath)
		return tmpBlobPath, nil
	}

	// Slow path: acquire a per-digest flock before running qemu-img
	// convert. Without this, N concurrent converters of identical
	// content each redo a full (potentially tens-of-seconds) convert
	// pass and throw all but one result away. The loser of the race
	// wakes up after Unlock, sees the blob already exists, and returns
	// an empty tmpBlobPath so commit proceeds directly to the index
	// update without redoing the convert.
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

// convertToQcow2 runs qemu-img convert to produce a qcow2 v3 (compat=1.1)
// blob at dst from src using the given source format. On failure, dst is
// removed and qemu-img stderr is included in the wrapped error.
func convertToQcow2(ctx context.Context, srcFormat, src, dst string) error {
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", //nolint:gosec // args are controlled
		"-f", srcFormat, "-O", "qcow2", "-o", "compat=1.1",
		src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(dst) //nolint:errcheck,gosec
		return fmt.Errorf("qemu-img convert: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// writeIndexEntry stats the blob at conf.BlobPath(digestHex) and
// registers a new ref → digest mapping in idx. Shared between commit
// (blob just placed) and commitExistingBlob (blob already cached). The
// stat is required to populate the entry's Size field.
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
