package vm

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestResizeSpecFromFlags(t *testing.T) {
	const gib = int64(1) << 30
	tests := []struct {
		name    string
		args    []string
		want    hypervisor.ResizeSpec
		wantErr string
	}{
		{name: "an omitted flag keeps its value", args: []string{"--memory", "8G"}, want: hypervisor.ResizeSpec{Memory: 8 * gib}},
		{name: "all three", args: []string{"--cpu", "4", "--memory", "8G", "--storage", "20G"}, want: hypervisor.ResizeSpec{CPU: 4, Memory: 8 * gib, Storage: 20 * gib}},
		{name: "an explicit zero vCPU count beside a valid size", args: []string{"--cpu", "0", "--memory", "8G"}, wantErr: "--cpu must be above zero"},
		{name: "an explicit zero size", args: []string{"--storage", "0"}, wantErr: "--storage must be above zero"},
		{name: "an empty size", args: []string{"--memory", ""}, wantErr: "--memory must be above zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().Int("cpu", 0, "")
			cmd.Flags().String("memory", "", "")
			cmd.Flags().String("storage", "", "")
			if err := cmd.Flags().Parse(tt.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}

			got, err := resizeSpecFromFlags(cmd)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("spec = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}
