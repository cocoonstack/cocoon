package vm

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestDiskAttachFlagGroups(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"path", []string{"--path", "/vols/d.raw", "--name", "d", "--readonly", "--directio", "off"}, false},
		{"from snapshot", []string{"--from-snapshot", "snap"}, false},
		{"from dir with name", []string{"--from-dir", "/exports/snap", "--name", "reader"}, false},
		{"no source", []string{"--name", "d"}, true},
		{"path and snapshot", []string{"--path", "/vols/d.raw", "--from-snapshot", "snap"}, true},
		{"snapshot and dir", []string{"--from-snapshot", "snap", "--from-dir", "/exports/snap"}, true},
		{"readonly snapshot", []string{"--from-snapshot", "snap", "--readonly"}, true},
		{"directio dir", []string{"--from-dir", "/exports/snap", "--directio", "off"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attach := diskSubcommand(t, "attach")
			if err := attach.ParseFlags(tt.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			err := attach.ValidateFlagGroups()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateFlagGroups() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func diskSubcommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	subs := buildDiskCommand(Handler{}).Commands()
	i := slices.IndexFunc(subs, func(c *cobra.Command) bool { return c.Name() == name })
	if i < 0 {
		t.Fatalf("disk %s subcommand missing", name)
	}
	return subs[i]
}
