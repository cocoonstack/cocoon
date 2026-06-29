package core

import (
	"encoding/json"
	"os"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func ReconcileState(vm *types.VM) string {
	if vm.State == types.VMStateRunning && !utils.IsProcessAlive(vm.PID) {
		return "stopped (stale)"
	}
	return string(vm.State)
}

func OutputJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func AddFormatFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("format", "o", "table", `output format: "table" or "json"`)
}

func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("output", "o", "", `emit "json" for machine-readable output`)
}

func WantJSON(cmd *cobra.Command) bool {
	out, _ := cmd.Flags().GetString("output")
	return out == "json"
}

// MaybeOutputJSON emits JSON iff --output=json; (true, _) means caller should stop logging.
func MaybeOutputJSON(cmd *cobra.Command, v any) (bool, error) {
	if !WantJSON(cmd) {
		return false, nil
	}
	return true, OutputJSON(v)
}

func OutputFormatted(cmd *cobra.Command, data any, tableFn func(w *tabwriter.Writer)) error {
	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return OutputJSON(data)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	tableFn(w)
	return w.Flush()
}

func FormatSize(bytes int64) string {
	return units.HumanSize(float64(bytes))
}
