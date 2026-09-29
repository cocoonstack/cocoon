package snapshot

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/cmd/cliutil/clitest"
	"github.com/cocoonstack/cocoon/config"
)

func TestListPrintsAnEmptyJSONArrayForAnEmptyStore(t *testing.T) {
	dir := t.TempDir()
	conf := &config.Config{RootDir: dir, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log")}
	h := Handler{ConfProvider: func() *config.Config { return conf }}
	for _, tt := range []struct {
		format string
		want   string
	}{
		{cliutil.FormatJSON, "[]\n"},
		{cliutil.FormatTable, "No snapshots found.\n"},
	} {
		list, _, err := Command(h).Find([]string{"list"})
		if err != nil {
			t.Fatalf("find list: %v", err)
		}
		if err := list.Flags().Set("format", tt.format); err != nil {
			t.Fatalf("set format: %v", err)
		}
		var runErr error
		got := clitest.CaptureStdout(t, func() { runErr = h.List(list, nil) })
		if runErr != nil {
			t.Fatalf("List --format %s: %v", tt.format, runErr)
		}
		if got != tt.want {
			t.Errorf("List --format %s printed %q, want %q", tt.format, got, tt.want)
		}
	}
}

func TestPrintNoSnapshots(t *testing.T) {
	for _, tt := range []struct {
		format string
		want   string
	}{
		{cliutil.FormatJSON, "[]\n"},
		{cliutil.FormatTable, "none\n"},
		{"", "none\n"},
	} {
		var err error
		got := clitest.CaptureStdout(t, func() { err = printNoSnapshots(tt.format, "none") })
		if err != nil || got != tt.want {
			t.Errorf("format %q: printed %q (err %v), want %q", tt.format, got, err, tt.want)
		}
	}
}
