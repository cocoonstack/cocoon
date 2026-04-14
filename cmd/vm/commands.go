package vm

import (
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
)

// Actions defines VM lifecycle operations.
type Actions interface {
	Create(cmd *cobra.Command, args []string) error
	Run(cmd *cobra.Command, args []string) error
	Clone(cmd *cobra.Command, args []string) error
	Start(cmd *cobra.Command, args []string) error
	Stop(cmd *cobra.Command, args []string) error
	List(cmd *cobra.Command, args []string) error
	Inspect(cmd *cobra.Command, args []string) error
	Console(cmd *cobra.Command, args []string) error
	RM(cmd *cobra.Command, args []string) error
	Restore(cmd *cobra.Command, args []string) error
	Debug(cmd *cobra.Command, args []string) error
	Status(cmd *cobra.Command, args []string) error
}

// Command builds the "vm" parent command with all subcommands.
func Command(h Actions) *cobra.Command {
	vmCmd := &cobra.Command{
		Use:   "vm",
		Short: "Manage virtual machines",
	}

	createCmd := &cobra.Command{
		Use:   "create [flags] IMAGE",
		Short: "Create a VM from an image",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Create,
	}
	addVMFlags(createCmd)

	runCmd := &cobra.Command{
		Use:   "run [flags] IMAGE",
		Short: "Create and start a VM from an image",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Run,
	}
	addVMFlags(runCmd)

	cloneCmd := &cobra.Command{
		Use:   "clone [flags] SNAPSHOT",
		Short: "Clone a new VM from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Clone,
	}
	addCloneFlags(cloneCmd)

	startCmd := &cobra.Command{
		Use:   "start VM [VM...]",
		Short: "Start created/stopped VM(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.Start,
	}

	stopCmd := &cobra.Command{
		Use:   "stop VM [VM...]",
		Short: "Stop running VM(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.Stop,
	}
	stopCmd.Flags().Bool("force", false, "force stop (skip graceful shutdown, immediate SIGTERM/SIGKILL)")
	stopCmd.Flags().Int("timeout", 0, "ACPI shutdown timeout in seconds (0 = use config default)")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List VMs with status",
		RunE:    h.List,
	}
	cmdcore.AddFormatFlag(listCmd)

	inspectCmd := &cobra.Command{
		Use:   "inspect VM",
		Short: "Show detailed VM info (JSON)",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Inspect,
	}

	consoleCmd := &cobra.Command{
		Use:   "console VM",
		Short: "Attach interactive console to a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Console,
	}
	consoleCmd.Flags().String("escape-char", "^]", "escape character (single char or ^X caret notation)")

	rmCmd := &cobra.Command{
		Use:   "rm [flags] VM [VM...]",
		Short: "Delete VM(s) (--force to stop running VMs first)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.RM,
	}
	rmCmd.Flags().Bool("force", false, "force delete running VMs")

	restoreCmd := &cobra.Command{
		Use:   "restore [flags] VM SNAPSHOT",
		Short: "Restore a running VM to a previous snapshot",
		Args:  cobra.ExactArgs(2),
		RunE:  h.Restore,
	}
	restoreCmd.Flags().Int("cpu", 0, "boot CPUs (0 = keep current)")
	restoreCmd.Flags().String("memory", "", "memory size (empty = keep current)")
	restoreCmd.Flags().String("storage", "", "COW disk size (empty = keep current)")

	debugCmd := &cobra.Command{
		Use:   "debug [flags] IMAGE",
		Short: "Generate hypervisor launch command (dry run)",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Debug,
	}
	addVMFlags(debugCmd)
	debugCmd.Flags().Int("max-cpu", 8, "max CPUs")           //nolint:mnd
	debugCmd.Flags().Int("balloon", 0, "balloon size in MB") //nolint:mnd
	debugCmd.Flags().String("cow", "", "COW disk path")
	debugCmd.Flags().String("ch", "cloud-hypervisor", "cloud-hypervisor binary path")

	statusCmd := &cobra.Command{
		Use:   "status [VM...]",
		Short: "Watch VM status in real time",
		RunE:  h.Status,
	}
	statusCmd.Flags().IntP("interval", "n", 5, "poll interval in seconds") //nolint:mnd
	statusCmd.Flags().Bool("event", false, "event stream mode (append changes instead of refreshing)")
	statusCmd.Flags().String("format", "", "output format: json (event mode only)")

	vmCmd.AddCommand(
		createCmd,
		runCmd,
		cloneCmd,
		startCmd,
		stopCmd,
		listCmd,
		inspectCmd,
		consoleCmd,
		rmCmd,
		restoreCmd,
		debugCmd,
		statusCmd,
	)
	return vmCmd
}

func addVMFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("fc", false, "use Firecracker backend instead of Cloud Hypervisor (OCI images only)")
	cmd.Flags().String("name", "", "VM name")
	cmd.Flags().Int("cpu", 2, "boot CPUs")                //nolint:mnd
	cmd.Flags().String("memory", "1G", "memory size")     //nolint:mnd
	cmd.Flags().String("storage", "10G", "COW disk size") //nolint:mnd
	cmd.Flags().Int("nics", 1, "number of network interfaces (0 = no network); multiple NICs with auto IP config only works for cloudimg; OCI images auto-configure only the last NIC, others require manual setup inside the guest")
	cmd.Flags().String("network", "", "CNI conflist name (empty = default); mutually exclusive with --bridge")
	cmd.Flags().String("bridge", "", "use TAP-on-bridge instead of CNI (value is bridge device, e.g. cni0); VM gets IP via DHCP from the bridge")
	cmd.Flags().Bool("windows", false, "Windows guest (UEFI boot, kvm_hyperv=on, no cidata)")
}

func addCloneFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "VM name (default: cocoon-clone-<id>)")
	cmd.Flags().Int("cpu", 0, "boot CPUs (0 = inherit from snapshot)")
	cmd.Flags().String("memory", "", "memory size (empty = inherit from snapshot)")
	cmd.Flags().String("storage", "", "COW disk size (empty = inherit from snapshot)")
	cmd.Flags().Int("nics", 0, "number of NICs (0 = inherit from snapshot)")
	cmd.Flags().String("network", "", "CNI conflist name (empty = inherit from source VM)")
	cmd.Flags().String("bridge", "", "use TAP-on-bridge instead of CNI (value is bridge device, e.g. cni0)")
}
