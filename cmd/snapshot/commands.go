package snapshot

import (
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
)

// Actions defines snapshot management operations.
type Actions interface {
	Save(cmd *cobra.Command, args []string) error
	List(cmd *cobra.Command, args []string) error
	Inspect(cmd *cobra.Command, args []string) error
	RM(cmd *cobra.Command, args []string) error
	Export(cmd *cobra.Command, args []string) error
	Import(cmd *cobra.Command, args []string) error
}

// Command builds the "snapshot" parent command with all subcommands.
func Command(h Actions) *cobra.Command {
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
	saveCmd.Flags().String("name", "", "snapshot name")
	saveCmd.Flags().String("description", "", "snapshot description")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all snapshots",
		RunE:    h.List,
	}
	cmdcore.AddFormatFlag(listCmd)
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
	exportCmd.Flags().StringP("output", "o", "", "output file path (default: <name-or-id>.tar.gz)")

	importCmd := &cobra.Command{
		Use:   "import [FILE]",
		Short: "Import a snapshot from a portable archive (file or stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE:  h.Import,
	}
	importCmd.Flags().String("name", "", "override snapshot name")
	importCmd.Flags().String("description", "", "override snapshot description")

	snapshotCmd.AddCommand(saveCmd, listCmd, inspectCmd, rmCmd, exportCmd, importCmd)
	return snapshotCmd
}
