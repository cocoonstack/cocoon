package snapshot

import (
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
)

func Command(h Handler) *cobra.Command {
	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage VM snapshots",
	}

	saveCmd := &cobra.Command{
		Use:   "save [flags] VM",
		Short: "Create a snapshot from a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Save,
	}
	cliutil.AddSnapshotNameFlags(saveCmd)

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all snapshots",
		RunE:    h.List,
	}
	cliutil.AddFormatFlag(listCmd)
	listCmd.Flags().String("vm", "", "only show snapshots belonging to this VM")

	inspectCmd := &cobra.Command{
		Use:   "inspect SNAPSHOT",
		Short: "Show detailed snapshot info (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Inspect,
	}

	rmCmd := &cobra.Command{
		Use:   "rm SNAPSHOT [SNAPSHOT...]",
		Short: "Delete snapshot(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.RM,
	}

	exportCmd := &cobra.Command{
		Use:   "export SNAPSHOT",
		Short: "Export a snapshot to a portable archive file",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Export,
	}
	exportCmd.Flags().StringP("output", "o", "", "output file path (default: <name-or-id>.tar)")
	exportCmd.Flags().Bool("gzip", false, "compress output with gzip")
	exportCmd.Flags().String("to-dir", "", "export into a directory (must be empty/absent) instead of a tar; pairs with 'vm clone --from-dir'")
	exportCmd.MarkFlagsMutuallyExclusive("to-dir", "output")
	exportCmd.MarkFlagsMutuallyExclusive("to-dir", "gzip")

	importCmd := &cobra.Command{
		Use:   "import [FILE]",
		Short: "Import a snapshot from a portable archive (file or stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE:  h.Import,
	}
	importCmd.Flags().String("name", "", "override snapshot name")
	importCmd.Flags().String("description", "", "override snapshot description")

	pushCmd := &cobra.Command{
		Use:   "push SNAPSHOT REF",
		Short: "Push a snapshot to an OCI registry",
		Args:  cobra.ExactArgs(2),
		RunE:  h.Push,
	}
	pushCmd.Flags().Int("zstd-level", 0, "zstd-compress snapshot layers at this level (0 disables)")
	pushCmd.Flags().Int("chunk-size-mib", 0, "split snapshot files into chunks of this many MiB (0 disables)")
	pushCmd.Flags().Int("concurrency", 8, "parallel chunk upload/encoder workers")
	pushCmd.Flags().Int("memory-budget-mib", 9216, "snapshot push pipeline memory cap in MiB")

	snapshotCmd.AddCommand(saveCmd, listCmd, inspectCmd, rmCmd, exportCmd, importCmd, pushCmd)
	return snapshotCmd
}
