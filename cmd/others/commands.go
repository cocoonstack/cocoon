// Package others defines the top-level gc and version commands.
package others

import "github.com/spf13/cobra"

func Commands(h Handler) []*cobra.Command {
	gcCmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove unreferenced blobs, boot files, VM dirs, and optionally evict snapshots",
		Args:  cobra.NoArgs,
		RunE:  h.GC,
	}
	gcCmd.Flags().Bool("snapshot", false, "evict snapshots by LRU; bare flag = all non-pending, refine with --snapshot-keep/age/size")
	gcCmd.Flags().Int("snapshot-keep", 0, "keep at most N most-recently-accessed snapshots (requires --snapshot)")
	gcCmd.Flags().Duration("snapshot-age", 0, "evict snapshots last accessed before this duration, e.g. 720h (requires --snapshot)")
	gcCmd.Flags().String("snapshot-size", "", "evict oldest snapshots until total size ≤ this, e.g. 100GB (requires --snapshot)")
	gcCmd.Flags().Bool("snapshot-dry-run", false, "log which snapshots would be LRU-evicted without acting (requires --snapshot; does NOT cover other GC modules)")
	return []*cobra.Command{
		gcCmd,
		{
			Use:   "version",
			Short: "Show version, git revision, and build timestamp",
			Args:  cobra.NoArgs,
			RunE:  h.Version,
		},
	}
}
