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

// importQcow2File imports a qcow2 file already on disk under name.
//
// Flow:
//  1. Hash pass — read the user file once to compute the content digest.
//  2. If the blob is already cached (repeat import of the same file
//     under a new name), short-circuit via commitExistingBlob and only
//     write a new index entry. Zero disk writes.
//  3. Otherwise, clone pass — seek the user file back to start, copy
//     it into a cocoon temp, and hand off to commit. The user's file
//     is never moved or modified.
//
// The two-pass cost (two full reads of the user file on the non-cached
// path) is deliberate: a single-pass "read + clone + hash" variant
// would always write the full clone to the temp directory even when
// the blob is already cached, wasting the disk write and potentially
// failing on temp-space exhaustion for large images.
func importQcow2File(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, filePath string) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	srcFile, err := os.Open(filePath) //nolint:gosec // filePath is caller input
	if err != nil {
		return fmt.Errorf("import %s: %w", filePath, err)
	}
	defer srcFile.Close() //nolint:errcheck,gosec

	// Sniff-first via ReadAt(0) — doesn't touch the file offset, so the
	// subsequent io.Copy still starts at byte 0.
	if err = sniffImageSource(srcFile); err != nil {
		return fmt.Errorf("import %s: %w", filePath, err)
	}

	// Pass 1: hash the user file (read-only).
	h := sha256.New()
	if _, err = io.Copy(h, srcFile); err != nil {
		return fmt.Errorf("hash %s: %w", filePath, err)
	}
	digestHex := hex.EncodeToString(h.Sum(nil))
	logger.Debugf(ctx, "hashed %s -> sha256:%s", filePath, digestHex[:12])

	// Cached fast path: blob already present, just register the new ref.
	if utils.ValidFile(conf.BlobPath(digestHex)) {
		if err = commitExistingBlob(ctx, conf, store, name, digestHex, tracker); err != nil {
			return err
		}
		logger.Infof(ctx, "import complete (cached): %s -> sha256:%s", name, digestHex)
		return nil
	}

	// Pass 2: blob missing, clone the user file into a cocoon temp and
	// hand off to commit. Seek back to the start for the second read.
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

	if _, err := io.Copy(tmpFile, srcFile); err != nil {
		return fmt.Errorf("copy %s: %w", filePath, err)
	}

	if err := commit(ctx, conf, store, name, tracker, tmpPath, digestHex); err != nil {
		return err
	}
	logger.Infof(ctx, "import complete: %s -> sha256:%s", name, digestHex)
	return nil
}

// importQcow2Reader buffers a stream to a cocoon-owned temp file, then
// imports it under name.
//
// The stream's first 8 bytes are sniffed BEFORE buffering the rest so
// a bad upstream (HTML error page, gzip-wrapped image, etc.) is
// rejected without incurring a GB-scale write+hash pass.
func importQcow2Reader(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, r io.Reader) error {
	logger := log.WithFunc("cloudimg.import")

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload})

	// Sniff-first: peek the first 8 bytes off the reader and reject
	// obvious non-images before touching disk. The consumed bytes are
	// stitched back onto the reader via MultiReader so the subsequent
	// io.Copy sees the full stream.
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

// importQcow2Concat concatenates multiple source files into a cocoon-owned
// temp file and imports the result under name.
//
// The first file is sniffed inline during its copy pass — since the
// concatenated result's first 8 bytes always come from file[0], this is
// equivalent to sniffing the final tmp but fails fast before any I/O if
// file[0] is obviously not a disk image.
func importQcow2Concat(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, file ...string) error {
	logger := log.WithFunc("cloudimg.import")

	if len(file) == 0 {
		return errors.New("no qcow2 files provided")
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

	for i, filePath := range file {
		src, openErr := os.Open(filePath) //nolint:gosec
		if openErr != nil {
			return fmt.Errorf("open %s: %w", filePath, openErr)
		}
		// Sniff the first file inline — ReadAt(0) doesn't move the
		// offset, so the following io.Copy still starts at 0.
		if i == 0 {
			if sniffErr := sniffImageSource(src); sniffErr != nil {
				src.Close() //nolint:errcheck,gosec
				return fmt.Errorf("import %s: %w", name, sniffErr)
			}
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
