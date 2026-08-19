package images

import (
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
)

func Command(h Handler) *cobra.Command {
	imageCmd := &cobra.Command{
		Use:   "image",
		Short: "Manage images",
	}
	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List locally stored images (all backends)",
		RunE:    h.List,
	}
	cliutil.AddFormatFlag(listCmd)

	importCmd := &cobra.Command{
		Use:   "import NAME [FILE...]",
		Short: "Import an image from a file or stdin",
		Long: `Import a local file or stdin stream as a cocoon image.

Type is auto-detected from magic bytes (supports gzip-wrapped input):
  - qcow2 (QFI magic): converted to qcow2 v3 and stored as a cloud image
  - tar: converted to an EROFS layer in an OCI image

When FILE is omitted, data is read from stdin.
A raw qcow2 file on disk uses an optimized path that avoids a temp copy.
Multiple FILE arguments are treated as split qcow2 parts or multiple tar layers.`,
		Args: cobra.MinimumNArgs(1),
		RunE: h.Import,
	}

	exportCmd := &cobra.Command{
		Use:   "export IMAGE",
		Short: "Export a locally stored cloud image as qcow2",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Export,
	}
	exportCmd.Flags().StringP("output", "o", "", "output file path (default: <image>.qcow2; use - for stdout)")

	pullCmd := &cobra.Command{
		Use:   "pull IMAGE [IMAGE...]",
		Short: "Pull OCI image(s) or cloud image URL(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.Pull,
	}
	pullCmd.Flags().Bool("force", false, "bypass cache and always re-download (useful when a mutable tag was replaced upstream)")

	imageCmd.AddCommand(
		pullCmd,
		importCmd,
		exportCmd,
		listCmd,
		&cobra.Command{
			Use:   "rm ID [ID...]",
			Short: "Delete locally stored image(s)",
			Args:  cobra.MinimumNArgs(1),
			RunE:  h.RM,
		},
		&cobra.Command{
			Use:   "inspect IMAGE",
			Short: "Show detailed image info (JSON)",
			Args:  cobra.ExactArgs(1),
			RunE:  h.Inspect,
		},
	)
	return imageCmd
}
