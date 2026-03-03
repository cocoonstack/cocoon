package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/console"
	"github.com/projecteru2/cocoon/hypervisor"
	"github.com/projecteru2/cocoon/hypervisor/cloudhypervisor"
	"github.com/projecteru2/cocoon/network"
	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/utils"
)

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Create(cmd *cobra.Command, args []string) error {
	ctx, info, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.create")
	logger.Infof(ctx, "VM created: %s (name: %s, state: %s)", info.ID, info.Config.Name, info.State)
	logger.Infof(ctx, "start with: cocoon vm start %s", info.ID)
	return nil
}

func (h Handler) Run(cmd *cobra.Command, args []string) error {
	ctx, info, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.run")
	logger.Infof(ctx, "VM created: %s (name: %s)", info.ID, info.Config.Name)

	started, err := info.hyper.Start(ctx, []string{info.ID})
	if err != nil {
		return fmt.Errorf("start VM %s: %w", info.ID, err)
	}
	for _, id := range started {
		logger.Infof(ctx, "started: %s", id)
	}
	return nil
}

func (h Handler) Clone(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.clone")

	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	snapRef := args[0]
	cfg, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer stream.Close() //nolint:errcheck

	// Close stream on context cancellation so Ctrl+C doesn't hang.
	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	// Build VMConfig from flags (same flags as create/run).
	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, "")
	if err != nil {
		return err
	}

	vmID, err := utils.GenerateID()
	if err != nil {
		return fmt.Errorf("generate VM ID: %w", err)
	}
	if vmCfg.Name == "" {
		vmCfg.Name = "cocoon-clone-" + vmID[:8]
	}

	// Create network (same pattern as createVM).
	var (
		networkConfigs []*types.NetworkConfig
		netProvider    network.Network
	)
	nics, _ := cmd.Flags().GetInt("nics")
	if nics > 0 {
		var initErr error
		netProvider, initErr = cmdcore.InitNetwork(conf)
		if initErr != nil {
			return fmt.Errorf("init network: %w", initErr)
		}
		networkConfigs, err = netProvider.Config(ctx, vmID, nics, vmCfg)
		if err != nil {
			return fmt.Errorf("configure network: %w", err)
		}
	}

	logger.Infof(ctx, "cloning VM from snapshot %s ...", snapRef)

	vm, cloneErr := hyper.Clone(ctx, vmID, vmCfg, cfg, networkConfigs, stream)
	if cloneErr != nil {
		if netProvider != nil {
			if _, delErr := netProvider.Delete(ctx, []string{vmID}); delErr != nil {
				logger.Warnf(ctx, "rollback network for %s: %v", vmID, delErr)
			}
		}
		return fmt.Errorf("clone VM: %w", cloneErr)
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	return nil
}

func (h Handler) Start(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}

	// Pre-start: recover missing netns (e.g. after host reboot).
	if netProvider, netErr := cmdcore.InitNetwork(conf); netErr == nil {
		h.recoverNetwork(ctx, hyper, netProvider, args)
	}

	return batchVMCmd(ctx, "start", "started", hyper.Start, args)
}

// recoverNetwork recreates the network namespace and TC redirect for VMs
// whose netns was lost (e.g. after host reboot). Best-effort: failures are
// logged but do not block start — hyper.Start will report the real error.
func (h Handler) recoverNetwork(ctx context.Context, hyper hypervisor.Hypervisor, net network.Network, refs []string) {
	logger := log.WithFunc("cmd.recoverNetwork")
	for _, ref := range refs {
		vm, err := hyper.Inspect(ctx, ref)
		if err != nil || vm == nil || len(vm.NetworkConfigs) == 0 {
			continue
		}
		if net.Verify(ctx, vm.ID) == nil {
			continue // netns exists, no recovery needed
		}
		logger.Warnf(ctx, "netns missing for VM %s, recovering network", vm.ID)
		if _, recoverErr := net.Config(ctx, vm.ID, len(vm.NetworkConfigs), &vm.Config, vm.NetworkConfigs...); recoverErr != nil {
			logger.Warnf(ctx, "recover network for VM %s: %v (start will fail)", vm.ID, recoverErr)
		}
	}
}

func (h Handler) Stop(cmd *cobra.Command, args []string) error {
	ctx, hyper, err := h.initHyper(cmd)
	if err != nil {
		return err
	}
	return batchVMCmd(ctx, "stop", "stopped", hyper.Stop, args)
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, hyper, err := h.initHyper(cmd)
	if err != nil {
		return err
	}

	vms, err := hyper.List(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}

	slices.SortFunc(vms, func(a, b *types.VM) int { return a.CreatedAt.Compare(b.CreatedAt) })

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(vms)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSTATE\tCPU\tMEMORY\tSTORAGE\tIP\tIMAGE\tCREATED")
	for _, vm := range vms {
		state := cmdcore.ReconcileState(vm)
		ips := vmIPs(vm)
		storage := units.BytesSize(float64(vm.Config.Storage))
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			vm.ID,
			vm.Config.Name,
			state,
			vm.Config.CPU,
			units.BytesSize(float64(vm.Config.Memory)),
			storage,
			ips,
			vm.Config.Image,
			vm.CreatedAt.Local().Format(time.DateTime),
		)
	}
	w.Flush() //nolint:errcheck,gosec
	return nil
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, hyper, err := h.initHyper(cmd)
	if err != nil {
		return err
	}

	info, err := hyper.Inspect(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(info)
}

func (h Handler) Console(cmd *cobra.Command, args []string) error {
	ctx, hyper, err := h.initHyper(cmd)
	if err != nil {
		return err
	}
	ref := args[0]

	conn, err := hyper.Console(ctx, ref)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	escapeStr, _ := cmd.Flags().GetString("escape-char")
	escapeChar, err := console.ParseEscapeChar(escapeStr)
	if err != nil {
		return err
	}

	inFd := os.Stdin.Fd()
	if !term.IsTerminal(inFd) {
		return fmt.Errorf("stdin is not a terminal")
	}

	oldState, err := term.SetRawTerminal(inFd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer func() {
		_ = term.RestoreTerminal(inFd, oldState)
		fmt.Fprintf(os.Stderr, "\r\nDisconnected from %s.\r\n", ref)
	}()

	escapeDisplay := console.FormatEscapeChar(escapeChar)
	fmt.Fprintf(os.Stderr, "Connected to %s (escape sequence: %s.)\r\n", ref, escapeDisplay)

	rw, ok := conn.(io.ReadWriter)
	if !ok {
		return fmt.Errorf("console connection does not support writing")
	}

	// Propagate terminal resize to PTY-backed consoles (direct boot / OCI).
	if f, ok := conn.(*os.File); ok {
		cleanup := console.HandleResize(inFd, f.Fd())
		defer cleanup()
	}

	escapeKeys := []byte{escapeChar, '.'}
	if err := console.Relay(rw, escapeKeys); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}

// RM deletes VMs. hyper.Delete uses best-effort semantics: it logs successfully
// deleted VMs in the returned slice even when later deletions fail, so we always
// report the partial results before checking the error.
func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.rm")

	force, _ := cmd.Flags().GetBool("force")

	deleted, deleteErr := hyper.Delete(ctx, args, force)
	for _, id := range deleted {
		logger.Infof(ctx, "deleted VM: %s", id)
	}

	// Clean up network resources for successfully deleted VMs first,
	// even if hyper.Delete returned a partial error.
	if len(deleted) > 0 {
		if netProvider, initErr := cmdcore.InitNetwork(conf); initErr == nil {
			if _, delErr := netProvider.Delete(ctx, deleted); delErr != nil {
				logger.Warnf(ctx, "network cleanup: %v", delErr)
			}
		}
	}

	if deleteErr != nil {
		return fmt.Errorf("rm: %w", deleteErr)
	}
	if len(deleted) == 0 {
		logger.Info(ctx, "no VMs deleted")
	}
	return nil
}

func (h Handler) Debug(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, _, _, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, args[0])
	if err != nil {
		return err
	}

	maxCPU, _ := cmd.Flags().GetInt("max-cpu")
	balloon, _ := cmd.Flags().GetInt("balloon")
	cowPath, _ := cmd.Flags().GetString("cow")
	chBin, _ := cmd.Flags().GetString("ch")

	storageConfigs, boot, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return err
	}

	memoryMB := int(vmCfg.Memory >> 20)   //nolint:mnd
	cowSizeGB := int(vmCfg.Storage >> 30) //nolint:mnd
	if balloon == 0 {
		balloon = memoryMB / 2 //nolint:mnd
	}

	if boot.KernelPath != "" {
		printRunOCI(storageConfigs, boot, vmCfg.Name, vmCfg.Image, cowPath, chBin, vmCfg.CPU, maxCPU, memoryMB, balloon, cowSizeGB)
	} else {
		printRunCloudimg(storageConfigs, boot, vmCfg.Name, vmCfg.Image, cowPath, chBin, vmCfg.CPU, maxCPU, memoryMB, balloon, cowSizeGB)
	}
	return nil
}

// initHyper is the shared init for methods that only need the hypervisor.
func (h Handler) initHyper(cmd *cobra.Command) (context.Context, hypervisor.Hypervisor, error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return nil, nil, err
	}
	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return nil, nil, err
	}
	return ctx, hyper, nil
}

// createResult holds the output of createVM for Create/Run to consume.
type createResult struct {
	*types.VM
	hyper hypervisor.Hypervisor
}

// createVM is the shared logic for Create and Run: resolve image, create VM.
func (h Handler) createVM(cmd *cobra.Command, image string) (context.Context, *createResult, error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return nil, nil, err
	}
	backends, hyper, err := cmdcore.InitBackends(ctx, conf)
	if err != nil {
		return nil, nil, err
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, image)
	if err != nil {
		return nil, nil, err
	}

	storageConfigs, bootCfg, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return nil, nil, err
	}
	cmdcore.EnsureFirmwarePath(conf, bootCfg)

	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate VM ID: %w", err)
	}

	var (
		networkConfigs []*types.NetworkConfig
		netProvider    network.Network
	)
	nics, _ := cmd.Flags().GetInt("nics")
	if nics > 0 {
		var initErr error
		netProvider, initErr = cmdcore.InitNetwork(conf)
		if initErr != nil {
			return nil, nil, fmt.Errorf("init network: %w", initErr)
		}
		networkConfigs, err = netProvider.Config(ctx, vmID, nics, vmCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("configure network: %w", err)
		}
	}

	info, createErr := hyper.Create(ctx, vmID, vmCfg, storageConfigs, networkConfigs, bootCfg)
	if createErr != nil {
		if netProvider != nil {
			if _, delErr := netProvider.Delete(ctx, []string{vmID}); delErr != nil {
				log.WithFunc("cmd.createVM").Warnf(ctx, "rollback network for %s: %v", vmID, delErr)
			}
		}
		return nil, nil, fmt.Errorf("create VM: %w", createErr)
	}
	return ctx, &createResult{VM: info, hyper: hyper}, nil
}

func batchVMCmd(ctx context.Context, name, pastTense string, fn func(context.Context, []string) ([]string, error), refs []string) error {
	logger := log.WithFunc("cmd." + name)
	done, err := fn(ctx, refs)
	for _, id := range done {
		logger.Infof(ctx, "%s: %s", pastTense, id)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if len(done) == 0 {
		logger.Infof(ctx, "no VMs %s", strings.ToLower(pastTense))
	}
	return nil
}

func printRunOCI(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int) {
	if cowPath == "" {
		cowPath = fmt.Sprintf("cow-%s.raw", vmName)
	}

	var diskArgs []string
	for _, d := range configs {
		diskArgs = append(diskArgs,
			fmt.Sprintf("path=%s,readonly=on,direct=off,image_type=raw,num_queues=2,queue_size=256,serial=%s", d.Path, d.Serial))
	}
	diskArgs = append(diskArgs,
		fmt.Sprintf("path=%s,readonly=off,direct=off,sparse=on,image_type=raw,num_queues=2,queue_size=256,serial=%s", cowPath, cloudhypervisor.CowSerial))

	cocoonLayers := strings.Join(cloudhypervisor.ReverseLayerSerials(configs), ",")

	cmdline := fmt.Sprintf(
		"console=hvc0 loglevel=3 boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw",
		cocoonLayers, cloudhypervisor.CowSerial)

	fmt.Println("# Prepare COW disk")
	fmt.Printf("truncate -s %dG %s\n", cowSize, cowPath)
	fmt.Printf("mkfs.ext4 -F -m 0 -q -E lazy_itable_init=1,lazy_journal_init=1,discard %s\n", cowPath)
	fmt.Println()

	fmt.Printf("# Launch VM: %s (image: %s, boot: direct kernel)\n", vmName, image)
	fmt.Printf("%s \\\n", chBin)
	fmt.Printf("  --kernel %s \\\n", boot.KernelPath)
	fmt.Printf("  --initramfs %s \\\n", boot.InitrdPath)
	fmt.Printf("  --disk")
	for _, d := range diskArgs {
		fmt.Printf(" \\\n    \"%s\"", d)
	}
	fmt.Printf(" \\\n")
	fmt.Printf("  --cmdline \"%s\" \\\n", cmdline)
	printCommonCHArgs(cpu, maxCPU, memory, balloon)
}

func printRunCloudimg(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int) {
	if cowPath == "" {
		cowPath = fmt.Sprintf("cow-%s.qcow2", vmName)
	}

	basePath := configs[0].Path

	fmt.Println("# Prepare COW overlay")
	fmt.Printf("qemu-img create -f qcow2 -F qcow2 -b %s %s\n", basePath, cowPath)
	if cowSize > 0 {
		fmt.Printf("qemu-img resize %s %dG\n", cowPath, cowSize)
	}
	fmt.Println()

	fmt.Printf("# Launch VM: %s (image: %s, boot: UEFI firmware)\n", vmName, image)
	fmt.Printf("%s \\\n", chBin)
	fmt.Printf("  --firmware %s \\\n", boot.FirmwarePath)
	fmt.Printf("  --disk \\\n")
	fmt.Printf("    \"path=%s,readonly=off,direct=off,image_type=qcow2,backing_files=on,num_queues=2,queue_size=256\" \\\n", cowPath)
	printCommonCHArgs(cpu, maxCPU, memory, balloon)
}

// vmIPs extracts a comma-separated IP string from a VM's NetworkConfigs.
func vmIPs(vm *types.VM) string {
	var ips []string
	for _, nc := range vm.NetworkConfigs {
		if nc != nil && nc.Network != nil && nc.Network.IP != "" {
			ips = append(ips, nc.Network.IP)
		}
	}
	if len(ips) == 0 {
		return "-"
	}
	return strings.Join(ips, ",")
}

// printCommonCHArgs outputs CH args for manual debugging.
// --serial tty outputs to the current terminal for interactive debugging,
// which intentionally differs from the automated path (Console: Pty / Serial: Socket).
func printCommonCHArgs(cpu, maxCPU, memory, balloon int) {
	fmt.Printf("  --cpus boot=%d,max=%d \\\n", cpu, maxCPU)
	fmt.Printf("  --memory size=%dM \\\n", memory)
	fmt.Printf("  --rng src=/dev/urandom \\\n")
	fmt.Printf("  --balloon size=%dM,deflate_on_oom=on,free_page_reporting=on \\\n", balloon)
	fmt.Printf("  --watchdog \\\n")
	fmt.Printf("  --serial tty --console off\n")
}
