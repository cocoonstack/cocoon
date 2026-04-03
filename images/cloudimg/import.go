package cloudimg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

// IsQcow2File checks if a file starts with the qcow2 magic bytes "QFI\xfb".
func IsQcow2File(path string) bool {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 'Q' && magic[1] == 'F' && magic[2] == 'I' && magic[3] == 0xfb //nolint:gosec
}

func importQcow2File(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, filePath string) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	digestHex, err := hashFile(filePath)
	if err != nil {
		return fmt.Errorf("hash %s: %w", filePath, err)
	}
	logger.Debugf(ctx, "hashed %s -> sha256:%s", filePath, digestHex[:12])

	return convertAndCommit(ctx, conf, store, name, tracker, digestHex, filePath)
}

func importQcow2Reader(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, r io.Reader) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	tmpFile, err := os.CreateTemp(conf.TempDir(), "import-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), r); err != nil {
		tmpFile.Close() //nolint:errcheck,gosec
		return fmt.Errorf("copy to temp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "buffered stream -> sha256:%s", digestHex[:12])

	return convertAndCommit(ctx, conf, store, name, tracker, digestHex, tmpPath)
}

func convertAndCommit(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, digestHex, sourcePath string) error {
	logger := log.WithFunc("cloudimg.import")

	blobPath := conf.BlobPath(digestHex)
	var tmpBlobPath string

	if !utils.ValidFile(blobPath) {
		tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseConvert})

		format, detectErr := detectImageFormat(ctx, sourcePath)
		if detectErr != nil {
			return fmt.Errorf("detect format: %w", detectErr)
		}
		logger.Debugf(ctx, "detected source format: %s", format)

		tmpBlob, createErr := os.CreateTemp(conf.TempDir(), ".tmp-*.qcow2")
		if createErr != nil {
			return fmt.Errorf("create temp blob: %w", createErr)
		}
		tmpBlobPath = tmpBlob.Name()
		tmpBlob.Close() //nolint:errcheck,gosec

		cmd := exec.CommandContext(ctx, "qemu-img", "convert", //nolint:gosec
			"-f", format, "-O", "qcow2", "-o", "compat=1.1",
			sourcePath, tmpBlobPath)
		if out, convertErr := cmd.CombinedOutput(); convertErr != nil {
			os.Remove(tmpBlobPath) //nolint:errcheck,gosec
			return fmt.Errorf("qemu-img convert: %s: %w", strings.TrimSpace(string(out)), convertErr)
		}
		defer os.Remove(tmpBlobPath) //nolint:errcheck
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseCommit})

	if err := store.Update(ctx, func(idx *imageIndex) error {
		if tmpBlobPath != "" && !utils.ValidFile(blobPath) {
			if renameErr := os.Rename(tmpBlobPath, blobPath); renameErr != nil {
				return fmt.Errorf("rename blob: %w", renameErr)
			}
			if chmodErr := os.Chmod(blobPath, 0o444); chmodErr != nil { //nolint:gosec
				logger.Warnf(ctx, "chmod blob %s: %v", blobPath, chmodErr)
			}
		}

		info, statErr := os.Stat(blobPath)
		if statErr != nil {
			return fmt.Errorf("stat blob %s: %w", blobPath, statErr)
		}

		idx.Images[name] = &imageEntry{
			Ref:        name,
			ContentSum: images.NewDigest(digestHex),
			Size:       info.Size(),
			CreatedAt:  time.Now(),
		}
		return nil
	}); err != nil {
		return fmt.Errorf("update index: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDone})
	logger.Infof(ctx, "import complete: %s -> sha256:%s", name, digestHex)
	return nil
}

func importQcow2Concat(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, file ...string) error {
	logger := log.WithFunc("cloudimg.import")

	if len(file) == 0 {
		return fmt.Errorf("no qcow2 files provided")
	}
	for _, f := range file {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("file %s: %w", f, err)
		}
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	tmpFile, err := os.CreateTemp(conf.TempDir(), "import-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	h := sha256.New()
	w := io.MultiWriter(tmpFile, h)

	for _, filePath := range file {
		f, openErr := os.Open(filePath) //nolint:gosec
		if openErr != nil {
			tmpFile.Close() //nolint:errcheck,gosec
			return fmt.Errorf("open %s: %w", filePath, openErr)
		}
		if _, copyErr := io.Copy(w, f); copyErr != nil {
			f.Close()       //nolint:errcheck,gosec
			tmpFile.Close() //nolint:errcheck,gosec
			return fmt.Errorf("copy %s: %w", filePath, copyErr)
		}
		f.Close() //nolint:errcheck,gosec
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "concatenated %d file(s) -> sha256:%s", len(file), digestHex[:12])

	return convertAndCommit(ctx, conf, store, name, tracker, digestHex, tmpPath)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
