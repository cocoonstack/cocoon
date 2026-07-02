// Package cliutil holds dependency-free CLI helpers (flags, table/JSON output) shared by cocoon and downstream CLIs.
package cliutil

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"
)

func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
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

func OutputJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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

func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}
