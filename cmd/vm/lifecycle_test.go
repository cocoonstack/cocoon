package vm

import (
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
)

func TestSeekToLastNLines(t *testing.T) {
	tests := []struct {
		name string
		body string
		n    int
		want string
	}{
		{name: "no trailing newline", body: "a\nb\nc", n: 2, want: "b\nc"},
		{name: "trailing newline", body: "a\nb\nc\n", n: 2, want: "b\nc\n"},
		{name: "more than available", body: "a\nb", n: 5, want: "a\nb"},
		{name: "one line trailing", body: "a\nb\nc\n", n: 1, want: "c\n"},
		{name: "empty", body: "", n: 3, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "log")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(tt.body); err != nil {
				t.Fatal(err)
			}
			if err := seekToLastNLines(f, tt.n); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyStopFlags(t *testing.T) {
	tests := []struct {
		name    string
		force   bool
		timeout int
		hasFlag bool
		want    int
	}{
		{"force wins", true, 0, true, -1},
		{"force beats timeout", true, 10, true, -1},
		{"timeout applies", false, 10, true, 10},
		{"defaults untouched", false, 0, true, 30},
		{"rm has no timeout flag", true, 0, false, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().Bool("force", tt.force, "")
			if tt.hasFlag {
				cmd.Flags().Int("timeout", tt.timeout, "")
			}
			conf := &config.Config{StopTimeoutSeconds: 30}
			applyStopFlags(conf, cmd)
			if conf.StopTimeoutSeconds != tt.want {
				t.Errorf("StopTimeoutSeconds = %d, want %d", conf.StopTimeoutSeconds, tt.want)
			}
		})
	}
}
