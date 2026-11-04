package vm

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
)

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
