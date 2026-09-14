package cloudimg

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	urlDownloadTimeout       = 30 * time.Minute
	maxDownloadBytes   int64 = 20 << 30
	progressInterval         = 1 << 20
	sniffLen                 = 8
)

// Doer is the client contract DownloadBlob needs; *http.Client and oras's auth.Client both satisfy it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// DownloadBlob writes url into dst and returns the sha256 hex of what landed: pullConns Range requests when the server honors them, else the single stream the probe already opened; the first bytes are sniffed before any bulk transfer.
func DownloadBlob(ctx context.Context, client Doer, url string, dst *os.File, pullConns int, tracker progress.Tracker) (string, error) {
	logger := log.WithFunc("cloudimg.DownloadBlob")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", sniffLen-1))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		return streamBody(resp, dst, tracker)
	case http.StatusPartialContent:
	default:
		return "", fmt.Errorf("http get %s: status %s", url, resp.Status)
	}
	size, ok := parseContentRangeSize(resp.Header.Get("Content-Range"))
	if !ok {
		logger.Debugf(ctx, "range reply for %s names no total size, falling back to a serial download", url)
		return downloadSerial(ctx, client, url, dst, tracker)
	}
	if size > maxDownloadBytes {
		return "", fmt.Errorf("download %s: exceeded max size (%d bytes)", url, maxDownloadBytes)
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, sniffLen))
	if err != nil {
		return "", fmt.Errorf("read range probe body for %s: %w", url, err)
	}
	if err := sniffHead(head); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	logger.Debugf(ctx, "downloading %s in %d parallel range(s)", url, pullConns)
	// the post-redirect URL spares every range request the redirect chain
	if err := downloadRangesParallel(ctx, client, resp.Request.URL.String(), dst, size, pullConns, tracker); err != nil {
		return "", err
	}
	return hashDigest(dst)
}

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

type countingWriter struct {
	w  io.Writer
	pc *progressCounter
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.pc.add(int64(n))
	return n, err
}

// pull commits url as a blob; the URL→blob mapping is idempotent (no-op when the blob already exists).
func pull(ctx context.Context, conf *Config, store *images.Store[imageEntry], url string, force bool, tracker progress.Tracker) error {
	logger := log.WithFunc("cloudimg.pull")

	if !force {
		var skip bool
		if err := store.View(ctx, func(idx *imageIndex) error {
			if _, entry, ok := images.LookupOne(idx.Images, url); ok {
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

	return withDownload(ctx, conf, url, tracker, func(_ *os.File, tmpPath, digestHex string) error {
		if err := commit(ctx, conf, store, url, tracker, tmpPath, digestHex); err != nil {
			return err
		}
		logger.Infof(ctx, "pull complete: %s -> sha256:%s", url, digestHex)
		return nil
	})
}

func withDownload(ctx context.Context, conf *Config, url string, tracker progress.Tracker, fn func(f *os.File, tmpPath, digestHex string) error) error {
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

func downloadToFile(ctx context.Context, url string, dst *os.File, tracker progress.Tracker, pullConns int) (string, error) {
	return DownloadBlob(ctx, &http.Client{Timeout: urlDownloadTimeout}, url, dst, pullConns, tracker)
}

// downloadSerial re-requests url without a Range header when a partial reply named no total size.
func downloadSerial(ctx context.Context, client Doer, url string, dst *os.File, tracker progress.Tracker) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http get %s: status %s", url, resp.Status)
	}
	return streamBody(resp, dst, tracker)
}

// streamBody copies one full response into dst, hashing as it goes; the head is sniffed before the first write.
func streamBody(resp *http.Response, dst *os.File, tracker progress.Tracker) (string, error) {
	url := resp.Request.URL.String()
	tracker.OnEvent(cloudimgProgress.Event{Phase: cloudimgProgress.PhaseDownload, BytesTotal: resp.ContentLength})
	body := bufio.NewReader(io.LimitReader(resp.Body, maxDownloadBytes+1))
	head, err := body.Peek(sniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err = sniffHead(head); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}

	h := sha256.New()
	pw := countingWriter{w: dst, pc: &progressCounter{total: max(resp.ContentLength, 0), tracker: tracker}}
	written, err := io.Copy(pw, io.TeeReader(body, h))
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

// downloadRangesParallel splits [0,size) into pullConns contiguous ranges downloaded concurrently into disjoint offsets of dst.
func downloadRangesParallel(ctx context.Context, client Doer, url string, dst *os.File, size int64, pullConns int, tracker progress.Tracker) error {
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

func downloadRange(ctx context.Context, client Doer, url string, dst *os.File, r [2]int64, pc *progressCounter) error {
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
