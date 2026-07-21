package utils

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SplitRanges divides [0,size) into up to n contiguous, inclusive-ended byte ranges.
func SplitRanges(size int64, n int) [][2]int64 {
	n = max(n, 1)
	chunk := (size + int64(n) - 1) / int64(n)
	ranges := make([][2]int64, 0, n)
	for start := int64(0); start < size; start += chunk {
		ranges = append(ranges, [2]int64{start, min(start+chunk-1, size-1)})
	}
	return ranges
}

// CopyRangeBody validates resp against the requested bytes=start-end range (status, span, length) and copies it into w — a mismatch would otherwise surface only at the later digest pass.
func CopyRangeBody(resp *http.Response, w io.Writer, start, end int64) error {
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range %d-%d: unexpected status %s", start, end, resp.Status)
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-%d/", start, end)) {
		return fmt.Errorf("range %d-%d: mismatched content-range %q", start, end, cr)
	}
	want := end - start + 1
	n, err := io.Copy(w, io.LimitReader(resp.Body, want))
	if err != nil {
		return fmt.Errorf("range %d-%d: %w", start, end, err)
	}
	if n != want {
		return fmt.Errorf("range %d-%d: short body %d of %d bytes", start, end, n, want)
	}
	return nil
}
