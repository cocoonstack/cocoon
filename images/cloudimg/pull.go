package cloudimg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// urlDownloadTimeout is the overall timeout for cloud image URL downloads.
	urlDownloadTimeout = 30 * time.Minute

	// maxDownloadBytes is the maximum allowed download size (20 GiB).
	maxDownloadBytes int64 = 20 << 30

	// report every 1 MiB
	progressInterval = 1 << 20
)

// progressWriter wraps an io.Writer and periodically emits download progress events.
type progressWriter struct {
	w          io.Writer
	written    int64
	total      int64
	tracker    progress.Tracker
	lastReport int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)
	if pw.written-pw.lastReport >= progressInterval {
		pw.lastReport = pw.written
		pw.tracker.OnEvent(cloudimgProgress.Event{
			Phase:      cloudimgProgress.PhaseDownload,
			BytesTotal: pw.total,
			BytesDone:  pw.written,
		})
	}
	return n, err
}

// pull downloads url and commits it as a blob under url. The URL→blob
// mapping is idempotent: a second pull of the same URL whose blob is
// still present is a no-op.
func pull(ctx context.Context, conf *Config, store storage.Store[imageIndex], url string, force bool, tracker progress.Tracker) error {
	logger := log.WithFunc("cloudimg.pull")

	// URL-level idempotency check (skipped when force is true).
	if !force {
		var skip bool
		if err := store.With(ctx, func(idx *imageIndex) error {
			if _, entry, ok := idx.Lookup(url); ok {
				blobPath := conf.BlobPath(entry.ContentSum.Hex())
				if utils.ValidFile(blobPath) {
					logger.Debugf(ctx, "image %s already cached, skipping", url)
					skip = true
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if skip {
			return nil
		}
	}

	return withDownload(ctx, conf, url, tracker, func(f *os.File, tmpPath, digestHex string) error {
		// Sniff using the still-open download handle — zero reopen.
		if err := sniffImageSource(f); err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
		if err := commit(ctx, conf, store, url, tracker, tmpPath, digestHex); err != nil {
			return err
		}
		logger.Infof(ctx, "pull complete: %s -> sha256:%s", url, digestHex)
		return nil
	})
}

func withDownload(
	ctx context.Context,
	conf *Config,
	url string,
	tracker progress.Tracker,
	fn func(f *os.File, tmpPath, digestHex string) error,
) error {
	tmpFile, err := os.CreateTemp(conf.TempDir(), "pull-*.img")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck,gosec
	defer tmpFile.Close()    //nolint:errcheck,gosec

	digestHex, err := downloadToFile(ctx, url, tmpFile, tracker)
	if err != nil {
		return err
	}
	return fn(tmpFile, tmpPath, digestHex)
}

func downloadToFile(ctx context.Context, url string, dst *os.File, tracker progress.Tracker) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create HTTP request: %w", err)
	}

	client := &http.Client{Timeout: urlDownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http get %s: status %d %s", url, resp.StatusCode, resp.Status)
	}

	contentLength := resp.ContentLength
	tracker.OnEvent(cloudimgProgress.Event{
		Phase:      cloudimgProgress.PhaseDownload,
		BytesTotal: contentLength,
	})

	h := sha256.New()
	limitedBody := io.LimitReader(resp.Body, maxDownloadBytes+1)
	reader := io.TeeReader(limitedBody, h)

	pw := &progressWriter{w: dst, total: contentLength, tracker: tracker}
	written, err := io.Copy(pw, reader)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if written > maxDownloadBytes {
		return "", fmt.Errorf("download %s: exceeded max size (%d bytes)", url, maxDownloadBytes)
	}

	if err := dst.Sync(); err != nil {
		return "", fmt.Errorf("sync temp file: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
