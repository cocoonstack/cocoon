package vm

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestChangedFlagsNamesOnlyWhatTheUserSet(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Int("nics", 1, "")
	cmd.Flags().Int("queue-size", 0, "")
	cmd.Flags().String("network", "", "")
	cmd.Flags().String("bridge", "", "")
	if err := cmd.Flags().Set("nics", "2"); err != nil {
		t.Fatalf("set nics: %v", err)
	}
	if err := cmd.Flags().Set("bridge", "cni0"); err != nil {
		t.Fatalf("set bridge: %v", err)
	}

	got := changedFlags(cmd, "nics", "queue-size", "network", "bridge")

	want := []string{"--nics", "--bridge"}
	if !slices.Equal(got, want) {
		t.Fatalf("changedFlags = %v, want %v", got, want)
	}
}

func TestChangedFlagsIsEmptyWhenNothingIsSet(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Int("nics", 1, "")

	if got := changedFlags(cmd, "nics"); len(got) != 0 {
		t.Fatalf("changedFlags = %v, want none for a default-valued flag", got)
	}
}
