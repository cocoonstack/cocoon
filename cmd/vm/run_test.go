package vm

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/types"
)

func TestPrepareCloneRejectsNoWatchdogWithFirecracker(t *testing.T) {
	cmd := &cobra.Command{}
	addCloneFlags(cmd)
	if err := cmd.Flags().Set("name", "clone"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("no-watchdog", "true"); err != nil {
		t.Fatal(err)
	}
	conf := &config.Config{UseFirecracker: true}
	snapshot := types.SnapshotConfig{Config: types.Config{
		CPU: 1, Memory: 512 << 20, Storage: 10 << 30,
	}}
	_, err := (Handler{}).prepareClone(t.Context(), cmd, conf, nil, snapshot)
	if err == nil || !strings.Contains(err.Error(), "--fc and --no-watchdog") {
		t.Fatalf("prepareClone() error = %v, want Firecracker incompatibility", err)
	}
}
