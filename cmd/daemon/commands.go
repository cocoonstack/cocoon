package daemon

import (
	"github.com/spf13/cobra"

	cocoond "github.com/cocoonstack/cocoon/daemon"
)

// Command builds the resident supervisor command.
func Command(h Handler) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Supervise cocoon-managed VMs: adopt live VMMs, converge exits, retry pending network quiesce",
		RunE:  h.Run,
	}
	cmd.Flags().Duration("reconcile-interval", cocoond.DefaultReconcileInterval, "full reconcile pass cadence")
	cmd.Flags().Duration("gc-interval", 0, "periodic gc cadence; 0 leaves gc a CLI verb")
	cmd.Flags().String("api-socket", "", "read-only API socket path (default: <run-dir>/cocoond.sock)")
	cmd.Flags().Bool("no-api", false, "disable the read-only API")
	cmd.Flags().Uint32("api-socket-mode", cocoond.DefaultAPISockMode, "read-only API socket permission bits")
	return cmd
}
