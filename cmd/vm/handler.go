package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/console"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Create(cmd *cobra.Command, args []string) error {
	ctx, vm, _, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.create")
	logger.Infof(ctx, "VM created: %s (name: %s, state: %s)", vm.ID, vm.Config.Name, vm.State)
	logger.Infof(ctx, "start with: cocoon vm start %s", vm.ID)
	return nil
}

func (h Handler) Run(cmd *cobra.Command, args []string) error {
	ctx, vm, hyper, err := h.createVM(cmd, args[0])
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.run")
	logger.Infof(ctx, "VM created: %s (name: %s)", vm.ID, vm.Config.Name)

	started, err := hyper.Start(ctx, []string{vm.ID})
	if err != nil {
		return fmt.Errorf("start VM %s: %w", vm.ID, err)
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

	// Try local fast path: skip tar encode/decode entirely.
	if da, ok := snapBackend.(snapshot.Direct); ok {
		if dcr, ok := hyper.(hypervisor.Direct); ok {
			return h.cloneDirect(ctx, cmd, conf, dcr, da, snapRef, logger)
		}
	}

	// Fallback: stream path.
	cfg, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	vmCfg, vmID, netProvider, networkConfigs, err := h.prepareClone(cmd, ctx, conf, cfg)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "cloning VM from snapshot %s ...", snapRef)

	vm, cloneErr := hyper.Clone(ctx, vmID, vmCfg, networkConfigs, cfg, stream)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm, networkConfigs)
	return nil
}

func (h Handler) cloneDirect(ctx context.Context, cmd *cobra.Command, conf *config.Config, dcr hypervisor.Direct, da snapshot.Direct, snapRef string, logger *log.Fields) error {
	dataDir, cfg, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", snapRef, err)
	}

	vmCfg, vmID, netProvider, networkConfigs, err := h.prepareClone(cmd, ctx, conf, cfg)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "cloning VM from snapshot %s (direct) ...", snapRef)

	vm, cloneErr := dcr.DirectClone(ctx, vmID, vmCfg, networkConfigs, cfg, dataDir)
	if cloneErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return fmt.Errorf("clone VM: %w", cloneErr)
	}

	logger.Infof(ctx, "VM cloned: %s (name: %s)", vm.ID, vm.Config.Name)
	printPostCloneHints(vm, networkConfigs)
	return nil
}

func (h Handler) prepareClone(cmd *cobra.Command, ctx context.Context, conf *config.Config, cfg *types.SnapshotConfig) (*types.VMConfig, string, network.Network, []*types.NetworkConfig, error) {
	vmCfg, err := cmdcore.CloneVMConfigFromFlags(cmd, cfg)
	if err != nil {
		return nil, "", nil, nil, err
	}

	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("generate VM ID: %w", err)
	}
	if vmCfg.Name == "" {
		vmCfg.Name = "cocoon-clone-" + vmID[:8]
	}

	nics, _ := cmd.Flags().GetInt("nics")
	if nics == 0 {
		nics = cfg.NICs
	}
	if nics < cfg.NICs {
		return nil, "", nil, nil, fmt.Errorf("--nics %d below snapshot minimum %d", nics, cfg.NICs)
	}

	netProvider, networkConfigs, err := initNetwork(ctx, conf, vmID, nics, vmCfg)
	if err != nil {
		return nil, "", nil, nil, err
	}

	return vmCfg, vmID, netProvider, networkConfigs, nil
}

// restoreDirect attempts the direct restore path. Returns (false, nil) if
// the backends don't support it, so the caller falls through to the stream path.
func (h Handler) restoreDirect(ctx context.Context, snapRef, vmRef string, vmCfg *types.VMConfig, snapBackend snapshot.Snapshot, hyper hypervisor.Hypervisor, logger *log.Fields) (bool, error) {
	da, ok := snapBackend.(snapshot.Direct)
	if !ok {
		return false, nil
	}
	dcr, ok := hyper.(hypervisor.Direct)
	if !ok {
		return false, nil
	}

	dataDir, _, err := da.DataDir(ctx, snapRef)
	if err != nil {
		return true, fmt.Errorf("open snapshot: %w", err)
	}

	logger.Infof(ctx, "restoring VM %s from snapshot %s (direct) ...", vmRef, snapRef)
	result, err := dcr.DirectRestore(ctx, vmRef, vmCfg, dataDir)
	if err != nil {
		return true, fmt.Errorf("restore: %w", err)
	}

	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return true, nil
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

func (h Handler) Stop(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool("force")
	timeout, _ := cmd.Flags().GetInt("timeout")

	// --force: skip ACPI, immediate kill. --timeout: override config default.
	if force {
		conf.StopTimeoutSeconds = -1
	} else if timeout > 0 {
		conf.StopTimeoutSeconds = timeout
	}

	hyper, err := cmdcore.InitHypervisor(conf)
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

	sortVMs(vms)

	return cmdcore.OutputFormatted(cmd, vms, func(w *tabwriter.Writer) {
		printVMTable(w, vms)
	})
}

func (h Handler) Status(cmd *cobra.Command, args []string) error {
	ctx, hyper, err := h.initHyper(cmd)
	if err != nil {
		return err
	}

	interval, _ := cmd.Flags().GetInt("interval")
	if interval <= 0 {
		interval = 5 //nolint:mnd
	}
	eventMode, _ := cmd.Flags().GetBool("event")

	// nil watchCh is safe: a nil channel blocks forever in select,
	// so the loop degrades to ticker-only polling.
	var watchCh <-chan struct{}
	if w, ok := hyper.(hypervisor.Watchable); ok {
		watchCh, err = utils.WatchFile(ctx, w.WatchPath(), 200*time.Millisecond) //nolint:mnd
		if err != nil {
			return fmt.Errorf("watch: %w", err)
		}
	}

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	if eventMode {
		statusEventLoop(ctx, hyper, args, watchCh, ticker.C)
	} else {
		isTTY := term.IsTerminal(os.Stdout.Fd())
		statusRefreshLoop(ctx, hyper, args, watchCh, ticker.C, isTTY)
	}
	return nil
}

// runLoop calls fn immediately, then repeats on watch events or ticker ticks.
func runLoop(ctx context.Context, watchCh <-chan struct{}, tick <-chan time.Time, fn func()) {
	fn()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-watchCh:
			if !ok {
				return
			}
			fn()
		case <-tick:
			fn()
		}
	}
}

func statusRefreshLoop(ctx context.Context, hyper hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time, isTTY bool) {
	var prev []vmSnapshot
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hyper, filters)
		curr := snapshotAll(vms)
		if slices.Equal(prev, curr) {
			return
		}
		prev = curr
		if isTTY {
			fmt.Print("\033[H\033[2J") //nolint:errcheck
		}
		fmt.Printf("Every %s — press Ctrl+C to quit (%s)\n\n",
			time.Now().Format(time.TimeOnly), time.Now().Format(time.DateOnly))
		if len(vms) == 0 {
			fmt.Println("No VMs found.")
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		printVMTable(w, vms)
		_ = w.Flush()
	})
}

func statusEventLoop(ctx context.Context, hyper hypervisor.Hypervisor, filters []string, watchCh <-chan struct{}, tick <-chan time.Time) {
	fmt.Println("EVENT\tID\tNAME\tSTATE\tCPU\tMEMORY\tIP\tIMAGE") //nolint:errcheck

	prev := map[string]vmSnapshot{}
	runLoop(ctx, watchCh, tick, func() {
		vms := listAndFilter(ctx, hyper, filters)
		curr := make(map[string]vmSnapshot, len(vms))
		for _, vm := range vms {
			curr[vm.ID] = takeSnapshot(vm)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for id, snap := range curr {
			old, existed := prev[id]
			switch {
			case !existed:
				printEventRow(w, "ADDED", snap)
			case old != snap:
				printEventRow(w, "MODIFIED", snap)
			}
		}
		for id, snap := range prev {
			if _, exists := curr[id]; !exists {
				printEventRow(w, "DELETED", snap)
			}
		}
		_ = w.Flush()
		prev = curr
	})
}

type vmSnapshot struct {
	id, name, state, ip, image string
	cpu                        int
	memory                     int64
}

func takeSnapshot(vm *types.VM) vmSnapshot {
	return vmSnapshot{
		id:     vm.ID,
		name:   vm.Config.Name,
		state:  cmdcore.ReconcileState(vm),
		cpu:    vm.Config.CPU,
		memory: vm.Config.Memory,
		ip:     vmIPs(vm),
		image:  vm.Config.Image,
	}
}

func printEventRow(w *tabwriter.Writer, event string, snap vmSnapshot) {
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", //nolint:errcheck
		event, snap.id, snap.name, snap.state,
		snap.cpu, units.BytesSize(float64(snap.memory)),
		snap.ip, snap.image)
}

func listAndFilter(ctx context.Context, hyper hypervisor.Hypervisor, filters []string) []*types.VM {
	vms, err := hyper.List(ctx)
	if err != nil {
		log.WithFunc("status").Warnf(ctx, "list: %v", err)
		return nil
	}
	sortVMs(vms)
	if len(filters) == 0 {
		return vms
	}
	var result []*types.VM
	for _, vm := range vms {
		if matchesFilter(vm, filters) {
			result = append(result, vm)
		}
	}
	return result
}

func matchesFilter(vm *types.VM, filters []string) bool {
	for _, f := range filters {
		if vm.ID == f || vm.Config.Name == f {
			return true
		}
		// Prefix match requires ≥3 chars, consistent with utils.ResolveRef.
		if len(f) >= 3 && strings.HasPrefix(vm.ID, f) {
			return true
		}
	}
	return false
}

func snapshotAll(vms []*types.VM) []vmSnapshot {
	result := make([]vmSnapshot, len(vms))
	for i, vm := range vms {
		result[i] = takeSnapshot(vm)
	}
	return result
}

func sortVMs(vms []*types.VM) {
	slices.SortFunc(vms, func(a, b *types.VM) int { return a.CreatedAt.Compare(b.CreatedAt) })
}

func printVMTable(w *tabwriter.Writer, vms []*types.VM) {
	fmt.Fprintln(w, "ID\tNAME\tSTATE\tCPU\tMEMORY\tSTORAGE\tIP\tIMAGE\tCREATED") //nolint:errcheck
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			vm.ID, vm.Config.Name, cmdcore.ReconcileState(vm),
			vm.Config.CPU, units.BytesSize(float64(vm.Config.Memory)),
			units.BytesSize(float64(vm.Config.Storage)),
			vmIPs(vm), vm.Config.Image,
			vm.CreatedAt.Local().Format(time.DateTime))
	}
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
	info.State = types.VMState(cmdcore.ReconcileState(info))
	return cmdcore.OutputJSON(info)
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
				return fmt.Errorf("VM(s) deleted but network cleanup failed: %w", delErr)
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

func (h Handler) Restore(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.restore")

	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	vmRef := args[0]
	snapRef := args[1]

	// Verify the snapshot belongs to this VM.
	vm, err := hyper.Inspect(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("inspect VM: %w", err)
	}
	snapInfo, err := snapBackend.Inspect(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("inspect snapshot: %w", err)
	}
	if _, ok := vm.SnapshotIDs[snapInfo.ID]; !ok {
		return fmt.Errorf("snapshot %s does not belong to VM %s", snapRef, vmRef)
	}

	// Validate NIC count matches.
	if snapInfo.NICs != len(vm.NetworkConfigs) {
		return fmt.Errorf("NIC count mismatch: VM has %d, snapshot has %d",
			len(vm.NetworkConfigs), snapInfo.NICs)
	}

	// Merge resource params: keep VM's current values, override with flags, validate >= snapshot.
	vmCfg, err := cmdcore.RestoreVMConfigFromFlags(cmd, vm, &snapInfo.SnapshotConfig)
	if err != nil {
		return err
	}

	// Try local fast path: skip tar encode/decode entirely.
	done, directErr := h.restoreDirect(ctx, snapRef, vmRef, vmCfg, snapBackend, hyper, logger)
	if done {
		return directErr
	}

	// Fallback: stream path.
	_, stream, err := snapBackend.Restore(ctx, snapRef)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	logger.Infof(ctx, "restoring VM %s from snapshot %s ...", vmRef, snapRef)

	result, err := hyper.Restore(ctx, vmRef, vmCfg, stream)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	logger.Infof(ctx, "VM %s restored (state: %s)", result.ID, result.State)
	return nil
}

func (h Handler) Debug(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
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

	printDebugRun(storageConfigs, boot, vmCfg.Name, vmCfg.Image, cowPath, chBin, vmCfg.CPU, maxCPU, memoryMB, balloon, cowSizeGB, vmCfg.Windows, boot.KernelPath != "")
	return nil
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

// createVM is the shared logic for Create and Run: resolve image, create VM.
func (h Handler) createVM(cmd *cobra.Command, image string) (context.Context, *types.VM, hypervisor.Hypervisor, error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	backends, hyper, err := cmdcore.InitBackends(ctx, conf)
	if err != nil {
		return nil, nil, nil, err
	}

	vmCfg, err := cmdcore.VMConfigFromFlags(cmd, image)
	if err != nil {
		return nil, nil, nil, err
	}

	storageConfigs, bootCfg, err := cmdcore.ResolveImage(ctx, backends, vmCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	if vmCfg.Windows && bootCfg.KernelPath != "" {
		return nil, nil, nil, fmt.Errorf("--windows requires cloudimg (UEFI boot), got OCI direct boot image")
	}
	cmdcore.EnsureFirmwarePath(conf, bootCfg)

	vmID, err := utils.GenerateID()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate VM ID: %w", err)
	}

	nics, _ := cmd.Flags().GetInt("nics")
	netProvider, networkConfigs, err := initNetwork(ctx, conf, vmID, nics, vmCfg)
	if err != nil {
		return nil, nil, nil, err
	}

	info, createErr := hyper.Create(ctx, vmID, vmCfg, storageConfigs, networkConfigs, bootCfg)
	if createErr != nil {
		rollbackNetwork(ctx, netProvider, vmID)
		return nil, nil, nil, fmt.Errorf("create VM: %w", createErr)
	}
	return ctx, info, hyper, nil
}

// initNetwork sets up network for a new VM. Returns nil provider and configs when nics == 0.
func initNetwork(ctx context.Context, conf *config.Config, vmID string, nics int, vmCfg *types.VMConfig) (network.Network, []*types.NetworkConfig, error) {
	if nics <= 0 {
		return nil, nil, nil
	}
	netProvider, err := cmdcore.InitNetwork(conf)
	if err != nil {
		return nil, nil, fmt.Errorf("init network: %w", err)
	}
	configs, err := netProvider.Config(ctx, vmID, nics, vmCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("configure network: %w", err)
	}
	return netProvider, configs, nil
}

// rollbackNetwork cleans up network resources on VM creation/clone failure.
func rollbackNetwork(ctx context.Context, netProvider network.Network, vmID string) {
	if netProvider == nil {
		return
	}
	if _, delErr := netProvider.Delete(ctx, []string{vmID}); delErr != nil {
		log.WithFunc("cmd.rollbackNetwork").Warnf(ctx, "rollback network for %s: %v", vmID, delErr)
	}
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

// printPostCloneHints outputs commands the user should run inside the guest
// after a clone to reconfigure network and release balloon memory.
func printPostCloneHints(vm *types.VM, networkConfigs []*types.NetworkConfig) {
	if vm.Config.Windows {
		fmt.Println()
		fmt.Println("Windows clone: NICs hot-swapped with new MAC addresses.")
		fmt.Println("  DHCP networks: no action needed.")
		fmt.Println("  Static IP: configure via SAC serial console (cocoon vm console):")
		fmt.Println("    https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md")
		fmt.Println()
		return
	}

	isCloudimg := slices.ContainsFunc(vm.StorageConfigs, func(sc *types.StorageConfig) bool {
		return strings.HasSuffix(sc.Path, ".qcow2")
	})

	fmt.Println()
	fmt.Println("Run inside the guest to finish setup:")
	fmt.Println()
	fmt.Println("  # Release memory for balloon")
	fmt.Println("  echo 3 > /proc/sys/vm/drop_caches")

	if isCloudimg {
		printCloudimgNetworkHints(networkConfigs)
	} else {
		printOCINetworkHints(vm, networkConfigs)
	}
	fmt.Println()
}

func printCloudimgNetworkHints(_ []*types.NetworkConfig) {
	// Cloudimg: NIC MACs are handled by hot-swap during clone (vm.remove-device +
	// vm.add-net while paused). Clean old network configs from snapshot COW first,
	// then cloud-init reinit writes correct MAC-based configs from the new cidata.
	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and reconfigure via cloud-init")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")
	fmt.Println("  cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init")
	fmt.Println("  cloud-init modules --mode=config && systemctl restart systemd-networkd")
}

type nicHint struct {
	mac, ip, gw string
	prefix      int
}

func printOCINetworkHints(vm *types.VM, networkConfigs []*types.NetworkConfig) {
	// OCI: no cloud-init, set hostname + write MAC-based systemd-networkd configs.
	// Two goals: immediate effect (systemctl restart) + reboot persistence.
	// MAC-based [Match] is essential because:
	//  1. After hot-swap, device names are unpredictable (eth2, eth3, ...).
	//  2. 10-<mac>.network takes priority over network.sh's 10-<mac>.network,
	//     so correct IPs survive guest reboots even if kernel ip= has stale values.
	fmt.Println()
	fmt.Printf("  # Set hostname\n")
	fmt.Printf("  hostnamectl set-hostname %s\n", vm.Config.Name)

	// Collect MACs from all NICs. Separate static (has IP) from DHCP (no IP).
	var staticNICs []nicHint
	var dhcpMACs []string
	for _, nc := range networkConfigs {
		if nc == nil || nc.Mac == "" {
			continue
		}
		if nc.Network != nil && nc.Network.IP != "" {
			staticNICs = append(staticNICs, nicHint{
				mac:    nc.Mac,
				ip:     nc.Network.IP,
				prefix: nc.Network.Prefix,
				gw:     nc.Network.Gateway,
			})
		} else {
			dhcpMACs = append(dhcpMACs, nc.Mac)
		}
	}

	if len(staticNICs) == 0 && len(dhcpMACs) == 0 {
		return
	}

	// Clean old snapshot network configs, then write MAC-based ones.
	fmt.Println()
	fmt.Println("  # Clean old network configs from snapshot and write new ones (MAC-based)")
	fmt.Println("  rm -f /etc/systemd/network/10-*.network")

	if len(staticNICs) > 0 {
		printBashArray("macs", staticNICs, func(n nicHint) string { return n.mac })
		printBashArray("addrs", staticNICs, func(n nicHint) string { return fmt.Sprintf("%s/%d", n.ip, n.prefix) })

		hasGW := slices.ContainsFunc(staticNICs, func(n nicHint) bool { return n.gw != "" })
		if hasGW {
			printBashArray("gws", staticNICs, func(n nicHint) string { return n.gw })
		}

		fmt.Println("  for i in \"${!macs[@]}\"; do")
		fmt.Println("    f=\"/etc/systemd/network/10-${macs[$i]//:/}.network\"")
		writeNet := `    printf '[Match]\nMACAddress=` + `%s\n\n[Network]\nAddress=%s\n' "${macs[$i]}" "${addrs[$i]}" > "$f"`
		fmt.Println(writeNet)
		if hasGW {
			writeGW := `    [ -n "${gws[$i]}" ] && printf 'Gateway=` + `%s\n' "${gws[$i]}" >> "$f"`
			fmt.Println(writeGW)
		}
		fmt.Println("  done")
	}

	if len(dhcpMACs) > 0 {
		fmt.Println("  # DHCP NICs")
		for _, mac := range dhcpMACs {
			sanitized := strings.ReplaceAll(mac, ":", "")
			writeDHCP := fmt.Sprintf(`  printf '[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n'`+` > "/etc/systemd/network/10-%s.network"`, mac, sanitized)
			fmt.Println(writeDHCP)
		}
	}

	fmt.Println("  systemctl restart systemd-networkd")
}

func printBashArray(name string, nics []nicHint, field func(nicHint) string) {
	fmt.Printf("  %s=(", name)
	for i, n := range nics {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Printf("'%s'", field(n))
	}
	fmt.Println(")")
}

func printDebugRun(configs []*types.StorageConfig, boot *types.BootConfig, vmName, image, cowPath, chBin string, cpu, maxCPU, memory, balloon, cowSize int, windows, directBoot bool) {
	if directBoot {
		if cowPath == "" {
			cowPath = fmt.Sprintf("cow-%s.raw", vmName)
		}

		debugConfigs := append(append([]*types.StorageConfig(nil), configs...),
			&types.StorageConfig{Path: cowPath, RO: false, Serial: cloudhypervisor.CowSerial})
		diskArgs := cloudhypervisor.DebugDiskCLIArgs(debugConfigs, cpu)

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
	} else {
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
		diskArgs := cloudhypervisor.DebugDiskCLIArgs([]*types.StorageConfig{{Path: cowPath, RO: false}}, cpu)
		fmt.Printf("    \"%s\" \\\n", diskArgs[0])
	}
	printCommonCHArgs(cpu, maxCPU, memory, balloon, windows)
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
func printCommonCHArgs(cpu, maxCPU, memory, balloon int, windows bool) {
	cpuExtra := ""
	if windows {
		cpuExtra = ",kvm_hyperv=on"
	}
	fmt.Printf("  --cpus boot=%d,max=%d%s \\\n", cpu, maxCPU, cpuExtra)
	fmt.Printf("  --memory size=%dM \\\n", memory)
	fmt.Printf("  --rng src=/dev/urandom \\\n")
	fmt.Printf("  --balloon size=%dM,deflate_on_oom=on,free_page_reporting=on \\\n", balloon)
	fmt.Printf("  --watchdog \\\n")
	fmt.Printf("  --serial tty --console off\n")
}
