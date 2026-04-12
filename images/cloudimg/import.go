package cloudimg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

// importQcow2File imports a local qcow2 via a hash pass and, if needed, a copy pass.
func importQcow2File(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, filePath string) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	srcFile, err := os.Open(filePath) //nolint:gosec // filePath is caller input
	if err != nil {
		return fmt.Errorf("import %s: %w", filePath, err)
	}
	defer srcFile.Close() //nolint:errcheck,gosec

	// ReadAt-based sniffing preserves the current file offset.
	if err = sniffImageSource(srcFile); err != nil {
		return fmt.Errorf("import %s: %w", filePath, err)
	}

	// First pass: hash the source file.
	h := sha256.New()
	if _, err = io.Copy(h, srcFile); err != nil {
		return fmt.Errorf("hash %s: %w", filePath, err)
	}
	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "hashed %s -> sha256:%s", filePath, digestHex[:12])

	// Cached fast path: just add the ref.
	if utils.ValidFile(conf.BlobPath(digestHex)) {
		if err = commitExistingBlob(ctx, conf, store, name, digestHex, tracker); err != nil {
			return err
		}
		logger.Infof(ctx, "import complete (cached): %s -> sha256:%s", name, digestHex)
		return nil
	}

	// Second pass: copy into a cocoon temp file.
	if _, err = srcFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", filePath, err)
	}

	tmpFile, err := os.CreateTemp(conf.TempDir(), "import-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck,gosec
	defer tmpFile.Close()    //nolint:errcheck,gosec

	// Rehash the copy pass so in-place source changes fail closed.
	verifyHash := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmpFile, verifyHash), srcFile); err != nil {
		return fmt.Errorf("copy %s: %w", filePath, err)
	}
	if verifyHex := hex.EncodeToString(verifyHash.Sum(nil)); verifyHex != digestHex {
		return fmt.Errorf("import %s: source file changed between hash and copy passes (hash was %s, copy is %s)",
			filePath, digestHex[:12], verifyHex[:12])
	}

	if err := commit(ctx, conf, store, name, tracker, tmpPath, digestHex); err != nil {
		return err
	}
	logger.Infof(ctx, "import complete: %s -> sha256:%s", name, digestHex)
	return nil
}

// importQcow2Reader buffers a stream to a cocoon-owned temp file, then imports it.
func importQcow2Reader(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, r io.Reader) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	// Peek before buffering, then stitch the bytes back with MultiReader.
	var head [8]byte
	n, err := io.ReadFull(r, head[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("import %s: read stream: %w", name, err)
	}
	if sniffErr := sniffHead(head[:n]); sniffErr != nil {
		return fmt.Errorf("import %s: %w", name, sniffErr)
	}
	full := io.MultiReader(bytes.NewReader(head[:n]), r)

	tmpFile, err := os.CreateTemp(conf.TempDir(), "import-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck,gosec
	defer tmpFile.Close()    //nolint:errcheck,gosec

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), full); err != nil {
		return fmt.Errorf("copy to temp: %w", err)
	}

	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "buffered stream -> sha256:%s", digestHex[:12])

	if err := commit(ctx, conf, store, name, tracker, tmpPath, digestHex); err != nil {
		return err
	}
	logger.Infof(ctx, "import complete: %s -> sha256:%s", name, digestHex)
	return nil
}

// importQcow2Concat concatenates multiple source files into one temp image.
func importQcow2Concat(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, file ...string) error {
	logger := log.WithFunc("cloudimg.import")

	if len(file) == 0 {
		return errors.New("no qcow2 files provided")
	}

	if sniffErr := sniffConcatHead(file); sniffErr != nil {
		return fmt.Errorf("import %s: %w", name, sniffErr)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	tmpFile, err := os.CreateTemp(conf.TempDir(), "import-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck,gosec
	defer tmpFile.Close()    //nolint:errcheck,gosec

	h := sha256.New()
	w := io.MultiWriter(tmpFile, h)

	for _, filePath := range file {
		src, openErr := os.Open(filePath) //nolint:gosec
		if openErr != nil {
			return fmt.Errorf("open %s: %w", filePath, openErr)
		}
		_, copyErr := io.Copy(w, src)
		src.Close() //nolint:errcheck,gosec
		if copyErr != nil {
			return fmt.Errorf("copy %s: %w", filePath, copyErr)
		}
	}

	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "concatenated %d file(s) -> sha256:%s", len(file), digestHex[:12])

	if err := commit(ctx, conf, store, name, tracker, tmpPath, digestHex); err != nil {
		return err
	}
	logger.Infof(ctx, "import complete: %s -> sha256:%s", name, digestHex)
	return nil
}

// sniffConcatHead peeks the first bytes of the logical concatenated stream.
func sniffConcatHead(file []string) error {
	var head [8]byte
	collected := 0
	for _, fp := range file {
		if collected >= len(head) {
			break
		}
		f, err := os.Open(fp) //nolint:gosec // path is caller input
		if err != nil {
			return fmt.Errorf("open %s: %w", fp, err)
		}
		n, readErr := f.ReadAt(head[collected:], 0)
		f.Close() //nolint:errcheck,gosec
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("peek %s: %w", fp, readErr)
		}
		collected += n
	}
	return sniffHead(head[:collected])
}
