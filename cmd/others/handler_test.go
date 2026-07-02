package others

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseSnapshotPolicy_Defaults(t *testing.T) {
	cmd := buildGCCmd()
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	p, err := parseSnapshotPolicy(cmd)
	if err != nil {
		t.Fatalf("default flags: %v", err)
	}
	if p.Enabled {
		t.Errorf("Enabled should default false")
	}
}

func TestParseSnapshotPolicy_NegativeRejected(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"negative keep", []string{"--snapshot", "--snapshot-keep=-1"}, "--snapshot-keep"},
		{"negative age", []string{"--snapshot", "--snapshot-age=-1h"}, "--snapshot-age"},
		{"negative size", []string{"--snapshot", "--snapshot-size=-100"}, "--snapshot-size"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cmd := buildGCCmd()
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			_, err := parseSnapshotPolicy(cmd)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err %q should mention %s", err, tt.want)
			}
		})
	}
}

func TestParseSnapshotPolicy_SubFlagRequiresSnapshot(t *testing.T) {
	cases := [][]string{
		{"--snapshot-keep=5"},
		{"--snapshot-age=24h"},
		{"--snapshot-size=10GB"},
		{"--snapshot-dry-run"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			cmd := buildGCCmd()
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			_, err := parseSnapshotPolicy(cmd)
			if err == nil || !strings.Contains(err.Error(), "requires --snapshot") {
				t.Errorf("want 'requires --snapshot' error, got %v", err)
			}
		})
	}
}

func TestParseSnapshotPolicy_HappyPath(t *testing.T) {
	cmd := buildGCCmd()
	if err := cmd.ParseFlags([]string{
		"--snapshot",
		"--snapshot-keep=10",
		"--snapshot-age=720h",
		"--snapshot-size=100GB",
		"--snapshot-dry-run",
	}); err != nil {
		t.Fatal(err)
	}
	p, err := parseSnapshotPolicy(cmd)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if !p.Enabled || !p.DryRun || p.KeepLast != 10 || p.MaxAge == 0 || p.MaxSize == 0 {
		t.Errorf("policy not populated: %+v", p)
	}
}

func buildGCCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "gc"}
	cmd.Flags().Bool("snapshot", false, "")
	cmd.Flags().Int("snapshot-keep", 0, "")
	cmd.Flags().Duration("snapshot-age", 0, "")
	cmd.Flags().String("snapshot-size", "", "")
	cmd.Flags().Bool("snapshot-dry-run", false, "")
	return cmd
}
