package vm

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
)

type Actions interface {
	Create(cmd *cobra.Command, args []string) error
	Run(cmd *cobra.Command, args []string) error
	Clone(cmd *cobra.Command, args []string) error
	Start(cmd *cobra.Command, args []string) error
	Stop(cmd *cobra.Command, args []string) error
	List(cmd *cobra.Command, args []string) error
	Inspect(cmd *cobra.Command, args []string) error
	Console(cmd *cobra.Command, args []string) error
	Exec(cmd *cobra.Command, args []string) error
	Logs(cmd *cobra.Command, args []string) error
	RM(cmd *cobra.Command, args []string) error
	Restore(cmd *cobra.Command, args []string) error
	Hibernate(cmd *cobra.Command, args []string) error
	Debug(cmd *cobra.Command, args []string) error
	Status(cmd *cobra.Command, args []string) error
	FsAttach(cmd *cobra.Command, args []string) error
	FsDetach(cmd *cobra.Command, args []string) error
	DeviceAttach(cmd *cobra.Command, args []string) error
	DeviceDetach(cmd *cobra.Command, args []string) error
	NetResize(cmd *cobra.Command, args []string) error
	Reseed(cmd *cobra.Command, args []string) error
}

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
	cliutil.AddOutputFlag(createCmd)

	runCmd := &cobra.Command{
		Use:   "run [flags] IMAGE",
		Short: "Create and start a VM from an image",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Run,
	}
	addVMFlags(runCmd)
	cliutil.AddOutputFlag(runCmd)

	cloneCmd := &cobra.Command{
		Use:   "clone [flags] [SNAPSHOT]",
		Short: "Clone a new VM from a snapshot (or a directory via --from-dir)",
		Args:  cobra.MaximumNArgs(1),
		RunE:  h.Clone,
	}
	addCloneFlags(cloneCmd)
	cliutil.AddOutputFlag(cloneCmd)

	startCmd := &cobra.Command{
		Use:   "start VM [VM...]",
		Short: "Start created/stopped VM(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.Start,
	}
	cliutil.AddOutputFlag(startCmd)

	stopCmd := &cobra.Command{
		Use:   "stop VM [VM...]",
		Short: "Stop running VM(s)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.Stop,
	}
	stopCmd.Flags().Bool("force", false, "force stop (skip graceful shutdown, immediate SIGTERM/SIGKILL)")
	stopCmd.Flags().Int("timeout", 0, "ACPI shutdown timeout in seconds (0 = use config default)")
	cliutil.AddOutputFlag(stopCmd)

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List VMs with status",
		RunE:    h.List,
	}
	cliutil.AddFormatFlag(listCmd)

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

	execCmd := &cobra.Command{
		Use:   "exec [flags] VM -- COMMAND [ARGS...]",
		Short: "Run a command inside a running VM via cocoon-agent (vsock)",
		Args:  cobra.MinimumNArgs(2),
		RunE:  h.Exec,
	}
	execCmd.Flags().StringArrayP("env", "e", nil, "extra env var KEY=VALUE (repeatable)")
	execCmd.Flags().BoolP("interactive", "i", false, "attach the caller's stdin to the command (otherwise stdin closes immediately)")

	reseedCmd := &cobra.Command{
		Use:   "reseed VM",
		Short: "Force a CRNG reseed inside the guest (fresh entropy over vsock)",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Reseed,
	}
	reseedCmd.Flags().Bool("machine-id", false, "also regenerate /etc/machine-id (use after clone, not restore)")

	logsCmd := &cobra.Command{
		Use:   "logs [flags] VM",
		Short: "Print the per-VM hypervisor log file",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Logs,
	}
	logsCmd.Flags().BoolP("follow", "f", false, "stream new log lines as they are written")
	logsCmd.Flags().Int("tail", 0, "show only the last N lines (0 = all)")

	rmCmd := &cobra.Command{
		Use:   "rm [flags] VM [VM...]",
		Short: "Delete VM(s) (--force to stop running VMs first)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  h.RM,
	}
	rmCmd.Flags().Bool("force", false, "force delete running VMs (immediate SIGTERM/SIGKILL, no graceful window)")
	cliutil.AddOutputFlag(rmCmd)

	restoreCmd := &cobra.Command{
		Use:   "restore [flags] VM [SNAPSHOT]",
		Short: "Restore a running or stopped VM to a previous snapshot (or a directory via --from-dir)",
		Args:  cobra.RangeArgs(1, 2),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			force, _ := cmd.Flags().GetBool("force")
			fromDir, _ := cmd.Flags().GetString("from-dir")
			if force && fromDir == "" {
				return fmt.Errorf("--force only applies with --from-dir")
			}
			return nil
		},
		RunE: h.Restore,
	}
	restoreCmd.Flags().Bool("on-demand", false, "use UFFD on-demand memory loading for faster restore (CH only; snapshot file must remain on disk)")
	restoreCmd.Flags().String("from-dir", "", "restore from a snapshot directory (must contain snapshot.json) instead of the local snapshot DB; mutually exclusive with positional SNAPSHOT")
	restoreCmd.Flags().Bool("force", false, "skip the snapshot-belongs-to-VM check (only meaningful with --from-dir; risk of restoring to an unrelated lineage)")
	cliutil.AddOutputFlag(restoreCmd)

	hibernateCmd := &cobra.Command{
		Use:   "hibernate [flags] VM",
		Short: "Atomically snapshot a running VM and stop it (resume with vm restore)",
		Args:  cobra.ExactArgs(1),
		RunE:  h.Hibernate,
	}
	hibernateCmd.Flags().String("name", "", "snapshot name")
	hibernateCmd.Flags().String("description", "", "snapshot description")

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
		Short: "Show VM status; --watch for refresh loop, --event for streaming",
		RunE:  h.Status,
	}
	statusCmd.Flags().IntP("interval", "n", 5, "poll interval in seconds (only with --watch or --event)") //nolint:mnd
	statusCmd.Flags().BoolP("watch", "w", false, "refresh-loop mode (full-screen redraw each tick); omit for one-shot snapshot")
	statusCmd.Flags().Bool("event", false, "event stream mode (append changes instead of refreshing); implies polling")
	statusCmd.Flags().String("format", "", "output format: json (one-shot + event modes; --watch always renders a table)")

	vmCmd.AddCommand(
		createCmd,
		runCmd,
		cloneCmd,
		startCmd,
		stopCmd,
		listCmd,
		inspectCmd,
		consoleCmd,
		execCmd,
		reseedCmd,
		logsCmd,
		rmCmd,
		restoreCmd,
		hibernateCmd,
		debugCmd,
		statusCmd,
		buildFsCommand(h),
		buildDeviceCommand(h),
		buildNetCommand(h),
	)
	return vmCmd
}

func buildNetCommand(h Actions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "net VM",
		Short: "Resize a running VM's NIC count (CH only); quiesce in-guest NIC state before reducing",
		Args:  cobra.ExactArgs(1),
		RunE:  h.NetResize,
	}
	cmd.Flags().Int("nics", -1, "target NIC count (required, >= 0)")
	_ = cmd.MarkFlagRequired("nics")
	cliutil.AddOutputFlag(cmd)
	return cmd
}

func buildFsCommand(h Actions) *cobra.Command {
	parent := &cobra.Command{
		Use:   "fs",
		Short: "Attach/detach a vhost-user-fs share to a running VM (CH only)",
	}

	attach := &cobra.Command{
		Use:   "attach VM",
		Short: "Attach a vhost-user-fs device to a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.FsAttach,
	}
	attach.Flags().String("socket", "", "absolute path to a virtiofsd unix socket (required)")
	attach.Flags().String("tag", "", "guest mount tag (required; also detach key)")
	attach.Flags().Int("num-queues", 0, "request queues (0 = default 1)")
	attach.Flags().Int("queue-size", 0, "queue depth (0 = default 1024)") //nolint:mnd
	_ = attach.MarkFlagRequired("socket")
	_ = attach.MarkFlagRequired("tag")
	cliutil.AddOutputFlag(attach)

	detach := &cobra.Command{
		Use:   "detach VM",
		Short: "Detach a vhost-user-fs device from a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.FsDetach,
	}
	detach.Flags().String("tag", "", "guest mount tag (required)")
	_ = detach.MarkFlagRequired("tag")
	cliutil.AddOutputFlag(detach)

	parent.AddCommand(attach, detach)
	return parent
}

func buildDeviceCommand(h Actions) *cobra.Command {
	parent := &cobra.Command{
		Use:   "device",
		Short: "Attach/detach a VFIO PCI passthrough device to a running VM (CH only)",
	}

	attach := &cobra.Command{
		Use:   "attach VM",
		Short: "Attach a VFIO PCI device to a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.DeviceAttach,
	}
	attach.Flags().String("pci", "", "BDF (01:00.0 / 0000:01:00.0) or sysfs path /sys/bus/pci/devices/<bdf>")
	attach.Flags().String("id", "", "optional device id; CH auto-generates if empty (must not start with cocoon-)")
	_ = attach.MarkFlagRequired("pci")
	cliutil.AddOutputFlag(attach)

	detach := &cobra.Command{
		Use:   "detach VM",
		Short: "Detach a VFIO PCI device from a running VM",
		Args:  cobra.ExactArgs(1),
		RunE:  h.DeviceDetach,
	}
	detach.Flags().String("id", "", "device id returned by attach (required)")
	_ = detach.MarkFlagRequired("id")
	cliutil.AddOutputFlag(detach)

	parent.AddCommand(attach, detach)
	return parent
}

func addVMFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("fc", false, "use Firecracker backend instead of Cloud Hypervisor (OCI images only)")
	cmd.Flags().String("name", "", "VM name")
	cmd.Flags().Int("cpu", 2, "boot CPUs")                //nolint:mnd
	cmd.Flags().String("memory", "1G", "memory size")     //nolint:mnd
	cmd.Flags().String("storage", "10G", "COW disk size") //nolint:mnd
	cmd.Flags().Int("nics", 1, "number of network interfaces (0 = no network); multiple NICs with auto IP config only works for cloudimg; OCI images auto-configure only the last NIC, others require manual setup inside the guest")
	cmd.Flags().Int("queue-size", 0, "virtio-net ring depth per queue (0 = default 512; tradeoff: larger improves download throughput, smaller improves RPC latency)") //nolint:mnd
	cmd.Flags().Int("disk-queue-size", 0, "virtio-blk ring depth per device (0 = default 512; CH only, ignored by FC)")                                                //nolint:mnd
	cmd.Flags().String("network", "", "CNI conflist name (empty = default); mutually exclusive with --bridge")
	cmd.Flags().String("bridge", "", "use TAP-on-bridge instead of CNI (value is bridge device, e.g. cni0); VM gets IP via DHCP from the bridge")
	cmd.Flags().String("user", "root", "guest username for cloud-init (cloudimg only)")
	cmd.Flags().String("password", "cocoon", "guest password for cloud-init (cloudimg only)")
	cmd.Flags().Bool("no-direct-io", false, "disable O_DIRECT on writable disks (use page cache instead; CH only)")
	cmd.Flags().Bool("windows", false, "Windows guest (UEFI boot, kvm_hyperv=on, no cidata)")
	cmd.Flags().Bool("shared-memory", false, "enable CH memory shared=on; required to attach vhost-user-fs later (CH only, fixed for VM lifetime)")
	cmd.Flags().StringArray("data-disk", nil, "extra data disk: size=20G[,name=...][,fstype=ext4|none][,mount=/mnt/x][,directio=on|off|auto]; repeatable")
}

func addCloneFlags(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "VM name (default: cocoon-clone-<id>)")
	cmd.Flags().Int("nics", 0, "override NIC count (omit to inherit from snapshot)")
	cmd.Flags().Int("queue-size", 0, "virtio-net ring depth per queue (0 = inherit from snapshot)")       //nolint:mnd
	cmd.Flags().Int("disk-queue-size", 0, "virtio-blk ring depth per device (0 = inherit from snapshot)") //nolint:mnd
	cmd.Flags().String("network", "", "CNI conflist name (empty = inherit from source VM)")
	cmd.Flags().String("bridge", "", "use TAP-on-bridge instead of CNI (value is bridge device, e.g. cni0)")
	cmd.Flags().Bool("no-direct-io", false, "disable O_DIRECT on writable disks (inherit from snapshot if not set)")
	cmd.Flags().Bool("on-demand", false, "use UFFD on-demand memory loading for faster clone (CH only; snapshot file must remain on disk)")
	cmd.Flags().Bool("pull", false, "auto-pull base image if not found locally (for cross-node clone)")
	cmd.Flags().String("from-dir", "", "clone from a snapshot directory (must contain snapshot.json) instead of the local snapshot DB; mutually exclusive with positional SNAPSHOT")
}
