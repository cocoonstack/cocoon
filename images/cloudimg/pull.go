package cloudimg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
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

	// progressInterval is how often download progress is reported (1 MiB).
	progressInterval = 1 << 20
)

// progressCounter emits PhaseDownload events every ~1 MiB; mutex-guarded so it serves both the serial writer and parallel range workers.
type progressCounter struct {
	mu         sync.Mutex
	written    int64
	lastReport int64
	total      int64
	tracker    progress.Tracker
}

func (pc *progressCounter) add(n int64) {
	pc.mu.Lock()
	pc.written += n
	report := pc.written-pc.lastReport >= progressInterval
	if report {
		pc.lastReport = pc.written
	}
	done := pc.written
	pc.mu.Unlock()
	// Emit outside the lock: the tracker callback is user-supplied and must not serialize the range workers.
	if report {
		pc.tracker.OnEvent(cloudimgProgress.Event{
			Phase:      cloudimgProgress.PhaseDownload,
			BytesTotal: pc.total,
			BytesDone:  done,
		})
	}
}

// countingWriter forwards writes to w while reporting byte counts to a shared progressCounter.
type countingWriter struct {
	w  io.Writer
	pc *progressCounter
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.pc.add(int64(n))
	return n, err
}

// rangeProbe carries the resolved URL and total size of a Range-capable source.
type rangeProbe struct {
	finalURL string
	size     int64
}

// pull commits url as a blob; the URL→blob mapping is idempotent (no-op when the blob already exists).
func pull(ctx context.Context, conf *Config, store storage.Store[imageIndex], url string, force bool, tracker progress.Tracker) error {
	logger := log.WithFunc("cloudimg.pull")

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
	tmpFile, tmpPath, cleanup, err := newTempImage(conf, "pull-*.img")
	if err != nil {
		return err
	}
	defer cleanup()

	digestHex, err := downloadToFile(ctx, url, tmpFile, tracker, conf.PullConns)
	if err != nil {
		return err
	}
	return fn(tmpFile, tmpPath, digestHex)
}

// downloadToFile downloads url into dst: pullConns concurrent Range connections when supported, a single stream otherwise.
func downloadToFile(ctx context.Context, url string, dst *os.File, tracker progress.Tracker, pullConns int) (string, error) {
	logger := log.WithFunc("cloudimg.downloadToFile")
	client := &http.Client{Timeout: urlDownloadTimeout}

	probe, err := probeRangeSupport(ctx, client, url)
	if err != nil {
		return "", err
	}
	if probe == nil {
		logger.Debugf(ctx, "range requests not supported for %s, falling back to serial download", url)
		return downloadSerial(ctx, client, url, dst, tracker)
	}
	if probe.size > maxDownloadBytes {
		return "", fmt.Errorf("download %s: exceeded max size (%d bytes)", url, maxDownloadBytes)
	}

	logger.Debugf(ctx, "downloading %s in %d parallel range(s)", url, pullConns)
	if err := downloadRangesParallel(ctx, client, probe.finalURL, dst, probe.size, pullConns, tracker); err != nil {
		return "", err
	}
	return hashDigest(dst)
}

// downloadSerial streams url into dst over one connection when the server lacks Range support or a usable size.
func downloadSerial(ctx context.Context, client *http.Client, url string, dst *os.File, tracker progress.Tracker) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create http request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http get %s: status %s", url, resp.Status)
	}

	contentLength := resp.ContentLength
	tracker.OnEvent(cloudimgProgress.Event{
		Phase:      cloudimgProgress.PhaseDownload,
		BytesTotal: contentLength,
	})

	h := sha256.New()
	limitedBody := io.LimitReader(resp.Body, maxDownloadBytes+1)
	reader := io.TeeReader(limitedBody, h)

	pw := countingWriter{w: dst, pc: &progressCounter{total: contentLength, tracker: tracker}}
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

// probeRangeSupport GETs Range: bytes=0-0 (nil = no usable Range support); the returned URL is post-redirect so range requests skip the redirect chain.
func probeRangeSupport(ctx context.Context, client *http.Client, url string) (*rangeProbe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create range probe request: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe range support for %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusPartialContent {
		return nil, nil
	}
	size, ok := parseContentRangeSize(resp.Header.Get("Content-Range"))
	if !ok {
		return nil, nil
	}
	return &rangeProbe{finalURL: resp.Request.URL.String(), size: size}, nil
}

// downloadRangesParallel splits [0,size) into pullConns contiguous ranges downloaded concurrently into disjoint offsets of dst.
func downloadRangesParallel(ctx context.Context, client *http.Client, url string, dst *os.File, size int64, pullConns int, tracker progress.Tracker) error {
	if err := dst.Truncate(size); err != nil {
		return fmt.Errorf("truncate temp file: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload, BytesTotal: size})
	pc := &progressCounter{total: size, tracker: tracker}

	ranges := utils.SplitRanges(size, pullConns)
	if _, err := utils.Map(ctx, ranges, func(ctx context.Context, _ int, r [2]int64) (struct{}, error) {
		return struct{}{}, downloadRange(ctx, client, url, dst, r, pc)
	}, pullConns); err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}

	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload, BytesTotal: size, BytesDone: size})
	return nil
}

func downloadRange(ctx context.Context, client *http.Client, url string, dst *os.File, r [2]int64, pc *progressCounter) error {
	start, end := r[0], r[1]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create range request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http get range %d-%d: %w", start, end, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	w := countingWriter{w: io.NewOffsetWriter(dst, start), pc: pc}
	return utils.CopyRangeBody(resp, w, start, end)
}

// hashDigest re-reads dst from disk: scattered parallel writes can't feed a streaming hasher, and hashing the file verifies what actually landed.
func hashDigest(dst *os.File) (string, error) {
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek temp file: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, dst); err != nil {
		return "", fmt.Errorf("hash temp file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseContentRangeSize extracts the total size from a "Content-Range: bytes 0-0/12345" header value.
func parseContentRangeSize(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 || i+1 >= len(v) {
		return 0, false
	}
	total := v[i+1:]
	if total == "*" {
		return 0, false
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil || size <= 0 {
		return 0, false
	}
	return size, true
}
