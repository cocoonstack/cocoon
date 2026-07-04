package utils

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSplitRanges(t *testing.T) {
	sizes := []int64{1, 7, 8, 9, 100, 257 << 10, (257 << 10) + 7}
	for _, size := range sizes {
		for _, n := range []int{0, 1, 3, 8, 100} {
			ranges := SplitRanges(size, n)
			if len(ranges) == 0 {
				t.Fatalf("size=%d n=%d: no ranges", size, n)
			}
			if ranges[0][0] != 0 {
				t.Errorf("size=%d n=%d: first start %d, want 0", size, n, ranges[0][0])
			}
			if last := ranges[len(ranges)-1][1]; last != size-1 {
				t.Errorf("size=%d n=%d: last end %d, want %d", size, n, last, size-1)
			}
			// contiguous + non-overlapping: each start is the previous end + 1
			for i := 1; i < len(ranges); i++ {
				if ranges[i][0] != ranges[i-1][1]+1 {
					t.Errorf("size=%d n=%d: gap/overlap at %d: %v after %v", size, n, i, ranges[i], ranges[i-1])
				}
			}
			if n >= 1 && len(ranges) > n {
				t.Errorf("size=%d n=%d: %d chunks exceeds n", size, n, len(ranges))
			}
		}
	}
	if got := SplitRanges(0, 4); len(got) != 0 {
		t.Errorf("size=0: got %v, want no ranges", got)
	}
}

func TestCopyRangeBody(t *testing.T) {
	const start, end = 10, 19
	body := "0123456789"
	tests := []struct {
		name         string
		status       int
		contentRange string
		body         string
		wantErr      string
	}{
		{"ok", http.StatusPartialContent, "bytes 10-19/100", body, ""},
		{"extra bytes beyond span are not copied", http.StatusPartialContent, "bytes 10-19/100", body + "EXTRA", ""},
		{"non-206 status", http.StatusOK, "", body, "unexpected status"},
		{"mismatched span", http.StatusPartialContent, "bytes 0-9/100", body, "mismatched content-range"},
		{"short body", http.StatusPartialContent, "bytes 10-19/100", "0123", "short body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tt.status,
				Status:     fmt.Sprintf("%d %s", tt.status, http.StatusText(tt.status)),
				Header:     http.Header{"Content-Range": {tt.contentRange}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}
			var buf bytes.Buffer
			err := CopyRangeBody(resp, &buf, start, end)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				if buf.String() != body {
					t.Errorf("copied %q, want %q", buf.String(), body)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
