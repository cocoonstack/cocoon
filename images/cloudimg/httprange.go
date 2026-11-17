package cloudimg

import (
	"cmp"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type objectVersion struct {
	size      int64
	validator string
}

// splitRanges divides [0,size) into up to n contiguous, inclusive-ended byte ranges.
func splitRanges(size int64, n int) [][2]int64 {
	n = max(n, 1)
	chunk := (size + int64(n) - 1) / int64(n)
	ranges := make([][2]int64, 0, n)
	for start := int64(0); start < size; start += chunk {
		ranges = append(ranges, [2]int64{start, min(start+chunk-1, size-1)})
	}
	return ranges
}

// copyRangeBody checks a 206 reply against start-end and the probed object version before copying it into w; a mismatch would otherwise surface only at the digest pass.
func copyRangeBody(resp *http.Response, w io.Writer, start, end int64, ver objectVersion) error {
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range %d-%d: unexpected status %s", start, end, resp.Status)
	}
	cr := resp.Header.Get("Content-Range")
	if !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-%d/", start, end)) {
		return fmt.Errorf("range %d-%d: mismatched content-range %q", start, end, cr)
	}
	if got := objectValidator(resp.Header); cr != fmt.Sprintf("bytes %d-%d/%d", start, end, ver.size) || (ver.validator != "" && got != ver.validator) {
		return fmt.Errorf("range %d-%d: upstream object changed during download (got %q %q, probe saw %d bytes %q); retry the pull", start, end, cr, got, ver.size, ver.validator)
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

func objectValidator(h http.Header) string {
	return cmp.Or(h.Get("ETag"), h.Get("Last-Modified"))
}
