// Package cliutil holds the flag and table/JSON output helpers the cocoon subcommands share.
package cliutil

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/types"
)

const (
	FormatTable = "table"
	FormatJSON  = "json"
)

// TableFunc renders the table body; the writer is flushed by the caller.
type TableFunc func(w *tabwriter.Writer)

type outputFlag string

func (o *outputFlag) String() string { return string(*o) }

func (o *outputFlag) Set(v string) error {
	if v != "" && v != FormatJSON {
		return fmt.Errorf("want %q", FormatJSON)
	}
	*o = outputFlag(v)
	return nil
}

func (o *outputFlag) Type() string { return "string" }

func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func GroupCommand(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
}

func AddFormatFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("format", "o", FormatTable, `output format: "table" or "json"`)
}

// Format reads --format and rejects a value outside the flag's enum.
func Format(cmd *cobra.Command) (string, error) {
	format := FlagStr(cmd, "format")
	return format, validateFormat(format)
}

func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().VarP(new(outputFlag), "output", "o", `emit "json" for machine-readable output`)
}

// AddSnapshotNameFlags registers the --name/--description pair shared by snapshot-creating commands.
func AddSnapshotNameFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "snapshot name")
	cmd.Flags().String("description", "", "snapshot description")
}

func WantJSON(cmd *cobra.Command) bool {
	return FlagStr(cmd, "output") == FormatJSON
}

func OutputJSON(v any) error {
	return json.MarshalEncode(jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  ")), v, json.Deterministic(true))
}

// MaybeOutputJSON emits JSON iff --output=json; (true, _) means caller should stop logging.
func MaybeOutputJSON(cmd *cobra.Command, v any) (bool, error) {
	if !WantJSON(cmd) {
		return false, nil
	}
	return true, OutputJSON(v)
}

func OutputFormatted(cmd *cobra.Command, data any, tableFn TableFunc) error {
	return OutputFormattedStr(FlagStr(cmd, "format"), data, tableFn)
}

func OutputFormattedStr(format string, data any, tableFn TableFunc) error {
	if err := validateFormat(format); err != nil {
		return err
	}
	if format == FormatJSON {
		return OutputJSON(data)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	tableFn(w)
	return w.Flush()
}

// FormatSize renders bytes in binary units (KiB/MiB/GiB).
func FormatSize(bytes int64) string {
	return units.BytesSize(float64(bytes))
}

func FlagBool(cmd *cobra.Command, name string) bool { v, _ := cmd.Flags().GetBool(name); return v }

func FlagInt(cmd *cobra.Command, name string) int { v, _ := cmd.Flags().GetInt(name); return v }

func FlagInt64(cmd *cobra.Command, name string) int64 { v, _ := cmd.Flags().GetInt64(name); return v }

func FlagStr(cmd *cobra.Command, name string) string { v, _ := cmd.Flags().GetString(name); return v }

// FlagSize reads a byte-size flag ("512M", "20G"); an empty value is 0.
func FlagSize(cmd *cobra.Command, name string) (int64, error) {
	s := FlagStr(cmd, name)
	if s == "" {
		return 0, nil
	}
	n, err := types.ParseSize(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s %q: %w", name, s, err)
	}
	return n, nil
}

func FlagStrings(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringArray(name)
	return v
}

func IsURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

func validateFormat(format string) error {
	switch format {
	case "", FormatTable, FormatJSON:
		return nil
	default:
		return fmt.Errorf("--format %q is invalid: want %q or %q", format, FormatTable, FormatJSON)
	}
}
