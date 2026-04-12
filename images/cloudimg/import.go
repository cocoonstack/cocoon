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

	// Hash the second pass too and compare to pass 1. If the user file
	// was modified in place between the two reads (TOCTOU), the two
	// digests disagree and we refuse to commit — otherwise the blob
	// would be stored and indexed under a digest that doesn't match
	// its own bytes. Zero extra I/O: hashing runs inline in the
	// io.Copy via MultiWriter.
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
// Before touching the tmp file, sniff the first 8 bytes of the
// conceptually-concatenated stream — accumulating bytes across shards
// if file[0] is shorter than the longest signature we care about
// (xz/7z need 6 bytes, qcow2 magic needs 4). This preserves the
// fail-fast behavior without the false-positive that arises when
// file[0] is only a few bytes and would otherwise be rejected as
// "too small" even though the joined stream is valid.
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

// sniffConcatHead reads up to 8 bytes from the concatenated prefix of
// the given shards (file[0]'s first bytes, falling through to file[1]
// etc. if earlier shards are short) and hands the accumulated prefix
// to sniffHead. Each shard is opened only long enough to fill the
// remaining prefix budget — tiny syscall cost relative to the
// full-file copy that follows, and sidesteps the "file[0] is 3 bytes
// of a valid qcow2 magic" false-positive rejection that results from
// sniffing file[0] in isolation.
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
