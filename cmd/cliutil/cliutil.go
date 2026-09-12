// Package cliutil holds dependency-free CLI helpers (flags, table/JSON output) shared by cocoon and downstream CLIs.
package cliutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"
)

const (
	FormatTable = "table"
	FormatJSON  = "json"
)

// TableFunc renders the table body; the writer is flushed by the caller.
type TableFunc func(w *tabwriter.Writer)

func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func AddFormatFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("format", "o", FormatTable, `output format: "table" or "json"`)
}

// Format reads --format and rejects a value outside the flag's enum.
func Format(cmd *cobra.Command) (string, error) {
	format, _ := cmd.Flags().GetString("format")
	return format, ValidateFormat(format)
}

func ValidateFormat(format string) error {
	switch format {
	case "", FormatTable, FormatJSON:
		return nil
	default:
		return fmt.Errorf("--format %q is invalid: want %q or %q", format, FormatTable, FormatJSON)
	}
}

func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("output", "o", "", `emit "json" for machine-readable output`)
}

// AddSnapshotNameFlags registers the --name/--description pair shared by snapshot-creating commands.
func AddSnapshotNameFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "snapshot name")
	cmd.Flags().String("description", "", "snapshot description")
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

func OutputFormatted(cmd *cobra.Command, data any, tableFn TableFunc) error {
	format, err := Format(cmd)
	if err != nil {
		return err
	}
	return OutputFormattedStr(format, data, tableFn)
}

func OutputFormattedStr(format string, data any, tableFn TableFunc) error {
	if err := ValidateFormat(format); err != nil {
		return err
	}
	if format == FormatJSON {
		return OutputJSON(data)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	tableFn(w)
	return w.Flush()
}

// FormatSize renders bytes in binary units (KiB/MiB/GiB) — the one convention for every table and progress line.
func FormatSize(bytes int64) string {
	return units.BytesSize(float64(bytes))
}

func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}
