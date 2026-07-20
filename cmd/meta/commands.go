package meta

import (
	"github.com/spf13/cobra"
)

func Command(h Handler) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "Meta store operations",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "init",
			Short: "Initialize a fresh sqlite meta store",
			RunE:  h.InitStore,
		},
		&cobra.Command{
			Use:   "convert",
			Short: "Convert existing metadata to the configured meta_backend",
			RunE:  h.Convert,
		},
		&cobra.Command{
			Use:   "backup <dest>",
			Short: "Back up the sqlite meta store to a single consistent file",
			Args:  cobra.ExactArgs(1),
			RunE:  h.Backup,
		},
	)
	return cmd
}
