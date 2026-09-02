package cloudimg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocoonstack/cocoon/progress"
	cloudimgProgress "github.com/cocoonstack/cocoon/progress/cloudimg"
)

func TestDownloadToFileParallelRange(t *testing.T) {
	data := testPayload(257 * 1024)
	server := httptest.NewServer(rangeHandler(data, nil))
	defer server.Close()

	dst := createTempFile(t)
	digest, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4)
	if err != nil {
		t.Fatalf("downloadToFile(): %v", err)
	}
	if want := sha256Hex(data); digest != want {
		t.Errorf("digest = %s, want %s", digest, want)
	}
	assertFileContent(t, dst, data)
}

func TestDownloadToFileFallsBackWhenRangeUnsupported(t *testing.T) {
	data := testPayload(64 * 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer server.Close()

	dst := createTempFile(t)
	digest, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4)
	if err != nil {
		t.Fatalf("downloadToFile(): %v", err)
	}
	if want := sha256Hex(data); digest != want {
		t.Errorf("digest = %s, want %s", digest, want)
	}
	assertFileContent(t, dst, data)
}

func TestDownloadToFileOneRangeFailureAbortsWhole(t *testing.T) {
	data := testPayload(4096)
	secondChunkStart := int64(len(data) / 2)
	server := httptest.NewServer(rangeHandler(data, func(start, _ int64) bool {
		return start == secondChunkStart
	}))
	defer server.Close()

	dst := createTempFile(t)
	if _, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 2); err == nil {
		t.Fatal("downloadToFile() = nil error, want failure from the broken range")
	}
}

func TestDownloadToFileRejectsOverCapSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", maxDownloadBytes+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer server.Close()

	dst := createTempFile(t)
	if _, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4); err == nil {
		t.Fatal("downloadToFile() = nil error, want rejection for exceeding max size")
	}
}

func TestDownloadToFileEmitsFinalProgressEvent(t *testing.T) {
	data := testPayload(64 * 1024)
	server := httptest.NewServer(rangeHandler(data, nil))
	defer server.Close()

	var lastEvent cloudimgProgress.Event
	tracker := progress.NewTracker(func(e cloudimgProgress.Event) { lastEvent = e })

	dst := createTempFile(t)
	if _, err := downloadToFile(t.Context(), server.URL, dst, tracker, 4); err != nil {
		t.Fatalf("downloadToFile(): %v", err)
	}
	if lastEvent.BytesDone != int64(len(data)) || lastEvent.BytesTotal != int64(len(data)) {
		t.Errorf("final event = %+v, want BytesDone=BytesTotal=%d", lastEvent, len(data))
	}
}

func TestDownloadToFileUnevenLastRange(t *testing.T) {
	data := testPayload(257*1024 + 7)
	server := httptest.NewServer(rangeHandler(data, nil))
	defer server.Close()

	dst := createTempFile(t)
	digest, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4)
	if err != nil {
		t.Fatalf("downloadToFile(): %v", err)
	}
	if want := sha256Hex(data); digest != want {
		t.Errorf("digest = %s, want %s", digest, want)
	}
	assertFileContent(t, dst, data)
}

func TestDownloadToFileFollowsRedirectForRanges(t *testing.T) {
	data := testPayload(64 * 1024)
	target := httptest.NewServer(rangeHandler(data, nil))
	defer target.Close()

	var frontRequests atomic.Int64
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frontRequests.Add(1)
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer front.Close()

	dst := createTempFile(t)
	digest, err := downloadToFile(t.Context(), front.URL, dst, progress.Nop, 4)
	if err != nil {
		t.Fatalf("downloadToFile(): %v", err)
	}
	if want := sha256Hex(data); digest != want {
		t.Errorf("digest = %s, want %s", digest, want)
	}
	assertFileContent(t, dst, data)

	if n := frontRequests.Load(); n != 1 {
		t.Errorf("front server saw %d requests, want 1 (probe only)", n)
	}
}

func TestDownloadToFileShortRangeBodyFails(t *testing.T) {
	data := testPayload(64 * 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		start, end, ok := parseTestRange(rangeHeader, int64(len(data)))
		if !ok {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if start == 0 || end-start < 2 {
			_, _ = w.Write(data[start : end+1])
			return
		}

		_, _ = w.Write(data[start : start+(end-start)/2])
		w.(http.Flusher).Flush()
	}))
	defer server.Close()

	dst := createTempFile(t)
	if _, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4); err == nil {
		t.Fatal("expected error for a short 206 body, got nil (zero-hole corruption)")
	}
}

func TestDownloadToFileMismatchedContentRangeFails(t *testing.T) {
	data := testPayload(64 * 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		start, end, ok := parseTestRange(rangeHeader, int64(len(data)))
		if !ok {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if start > 0 {
			start--
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	dst := createTempFile(t)
	if _, err := downloadToFile(t.Context(), server.URL, dst, progress.Nop, 4); err == nil {
		t.Fatal("expected error for mismatched content-range, got nil")
	}
}

// rangeHandler serves data with full HTTP Range support; fail, if non-nil, lets a test force a specific requested range to error out (simulating a mid-download server failure).
func rangeHandler(data []byte, fail func(start, end int64) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		start, end, ok := parseTestRange(rangeHeader, int64(len(data)))
		if !ok {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if fail != nil && fail(start, end) {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
}

func parseTestRange(h string, size int64) (start, end int64, ok bool) {
	h = strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(h, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

func testPayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func createTempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dst-*.img")
	if err != nil {
		t.Fatalf("CreateTemp(): %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func assertFileContent(t *testing.T, f *os.File, want []byte) {
	t.Helper()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek(): %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll(): %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("file size = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file content mismatch at byte %d: got %d, want %d", i, got[i], want[i])
		}
	}
}
