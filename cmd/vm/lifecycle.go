package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/console"
	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

const (
	// logHeadSigLen spans CH/FC's boot timestamp on line 1.
	logHeadSigLen = 64
	// logFollowDebounce coalesces fsnotify events before catch-up io.Copy fires.
	logFollowDebounce = 100 * time.Millisecond
)

type attachedDevices struct {
	Fs      []fs.Attached   `json:"fs,omitempty"`
	Devices []vfio.Attached `json:"devices,omitempty"`
	Disks   []disk.Attached `json:"disks,omitempty"`
}

type inspectOutput struct {
	*types.VM
	AttachedDevices *attachedDevices `json:"attached_devices,omitempty"`
}

func (h Handler) Start(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	for hyper, refs := range routed {
		h.recoverNetwork(ctx, conf, hyper, refs)
	}

	return batchRoutedCmd(ctx, cmd, "start", "started", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Start(ctx, refs)
	})
}

func (h Handler) Stop(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	applyStopFlags(conf, cmd)

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}
	return batchRoutedCmd(ctx, cmd, "stop", "stopped", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return h.stopAndQuiesce(ctx, conf, hyper, refs)
	})
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hyper, err := cmdcore.FindHypervisor(ctx, conf, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	info, err := hyper.Inspect(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	info.State = types.VMState(cmdcore.ReconcileState(info))

	out := inspectOutput{VM: info}
	if info.State == types.VMStateRunning {
		out.AttachedDevices = collectAttachedDevices(ctx, hyper, args[0])
	}
	return cliutil.OutputJSON(out)
}

func (h Handler) Console(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hyper, err := cmdcore.FindHypervisor(ctx, conf, args[0])
	if err != nil {
		return fmt.Errorf("console: %w", err)
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

func (h Handler) Logs(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	hyper, err := cmdcore.FindHypervisor(ctx, conf, args[0])
	if err != nil {
		return fmt.Errorf("logs: %w", err)
	}
	path, err := hyper.LogPath(ctx, args[0])
	if err != nil {
		return fmt.Errorf("logs: %w", err)
	}
	follow, _ := cmd.Flags().GetBool("follow")
	tail, _ := cmd.Flags().GetInt("tail")
	return streamLog(ctx, path, follow, tail)
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	applyStopFlags(conf, cmd)

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	if force {
		for hyper, refs := range routed {
			_, _ = h.stopAndQuiesce(ctx, conf, hyper, refs)
		}
	}

	const logTag = "cmd.vm.rm"
	allDeleted, lastErr := runRoutedLoop(ctx, logTag, "deleted", cliutil.WantJSON(cmd), routed,
		func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
			return hyper.Delete(ctx, refs, force)
		})

	// Network teardown now runs inside each VM's delete protocol, under its
	// lock (design §5) — no post-hoc cleanup pass.
	return finishRoutedCmd(ctx, cmd, logTag, "rm", "deleted", allDeleted, lastErr)
}

// recoverNetworkForStopped runs Start's network self-heal when the restore target is not running (hibernate resume after a host reboot).
func (h Handler) recoverNetworkForStopped(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, ref string) {
	vm, err := hyper.Inspect(ctx, ref)
	if err != nil || vm.State == types.VMStateRunning {
		return
	}
	h.recoverNetwork(ctx, conf, hyper, []string{vm.ID})
}

func (h Handler) recoverNetwork(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, refs []string) {
	logger := log.WithFunc("cmd.vm.recoverNetwork")

	// Lazy CNI; OK to skip for bridge-only setups.
	var cniProvider network.Network
	if p, err := cmdcore.InitNetwork(conf); err == nil {
		cniProvider = p
	}

	bridgeProviders := map[string]network.Network{}

	// Single List → byID map avoids one Inspect-per-ref under DB lock.
	all, err := hyper.List(ctx)
	if err != nil {
		logger.Warnf(ctx, "list VMs for recovery: %v", err)
		return
	}
	byID := make(map[string]*types.VM, len(all))
	for _, vm := range all {
		byID[vm.ID] = vm
	}

	for _, ref := range refs {
		vm := byID[ref]
		if vm == nil {
			continue
		}
		// A corrupt record (null NIC entry) must not grow fresh plumbing here; start will refuse it with a typed error.
		if err := types.ValidateNetworkConfigs(vm.NetworkConfigs); err != nil {
			logger.Warnf(ctx, "skip network recovery for VM %s: %v", vm.ID, err)
			continue
		}
		backend := vm.ResolvedNetBackend()
		if backend == "" {
			continue
		}
		if backend == types.BackendBridge && len(vm.NetworkConfigs) == 0 {
			continue
		}
		netProvider, provErr := providerForVM(conf, cniProvider, bridgeProviders, vm)
		if provErr != nil {
			logger.Warnf(ctx, "skip recovery for VM %s: %v", vm.ID, provErr)
			continue
		}
		recoverOne := func(vm *types.VM) {
			if netProvider.Verify(ctx, vm.ID, vm.NetworkConfigs) == nil {
				if err := netProvider.Unquiesce(ctx, vm.ID); err != nil {
					logger.Warnf(ctx, "unquiesce network for VM %s: %v", vm.ID, err)
				}
				return
			}
			logger.Warnf(ctx, "network missing for VM %s, recovering", vm.ID)
			if _, prepErr := netProvider.Prepare(ctx, vm.ID, &vm.Config); prepErr != nil {
				logger.Warnf(ctx, "prepare netns for VM %s: %v (start will fail)", vm.ID, prepErr)
				return
			}
			if len(vm.NetworkConfigs) == 0 {
				return
			}
			if _, recoverErr := netProvider.Add(ctx, vm.ID, &vm.Config, network.AddRecover(vm.NetworkConfigs)...); recoverErr != nil {
				logger.Warnf(ctx, "recover network for VM %s: %v (start will fail)", vm.ID, recoverErr)
			}
		}
		// Ops lock serializes verify-and-rebuild: two concurrent starts would both see missing plumbing, double-ADD, and the loser's rollback DEL would tear down the winner's network.
		if locker, ok := hyper.(hypervisor.Reserver); ok {
			unlock, lockErr := locker.LockVMOps(ctx, vm.ID)
			if lockErr != nil {
				logger.Warnf(ctx, "skip network recovery for VM %s: ops lock: %v", vm.ID, lockErr)
				continue
			}
			// Recheck under the lock from the fresh record: the pre-lock List snapshot may predate a completed rm or net resize.
			if fresh, inspectErr := hyper.Inspect(ctx, vm.ID); inspectErr == nil {
				recoverOne(fresh)
			}
			unlock()
		} else {
			recoverOne(vm)
		}
	}
}

func (h Handler) stopAndQuiesce(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
	stopped, err := hyper.Stop(ctx, refs)
	h.quiesceNetwork(ctx, conf, hyper, stopped)
	return stopped, err
}

// quiesceNetwork brings each stopped VM's host NICs down — Stop's counterpart to Start's recoverNetwork — so an idle TAP's TC redirect can't storm softirqs. Best-effort: a quiesce failure never blocks the stop.
func (h Handler) quiesceNetwork(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, ids []string) {
	if len(ids) == 0 {
		return
	}
	logger := log.WithFunc("cmd.vm.quiesceNetwork")

	var cniProvider network.Network
	if p, err := cmdcore.InitNetwork(conf); err == nil {
		cniProvider = p
	}
	bridgeProviders := map[string]network.Network{}

	for _, id := range ids {
		vm, err := hyper.Inspect(ctx, id)
		if err != nil {
			logger.Warnf(ctx, "inspect VM %s for quiesce: %v", id, err)
			continue
		}
		if vm == nil {
			continue
		}
		if vm.ResolvedNetBackend() == "" || len(vm.NetworkConfigs) == 0 {
			continue
		}
		netProvider, provErr := providerForVM(conf, cniProvider, bridgeProviders, vm)
		if provErr != nil {
			logger.Warnf(ctx, "skip quiesce for VM %s: %v", id, provErr)
			continue
		}
		if err := netProvider.Quiesce(ctx, id); err != nil {
			logger.Warnf(ctx, "quiesce network for VM %s: %v", id, err)
		}
	}
}

// applyStopFlags maps --force (-1 = immediate kill; #82: FC guests without i8042 never answer CtrlAltDel) and --timeout onto the stop window; rm has no --timeout flag, that read no-ops.
func applyStopFlags(conf *config.Config, cmd *cobra.Command) {
	force, _ := cmd.Flags().GetBool("force")
	timeout, _ := cmd.Flags().GetInt("timeout")
	switch {
	case force:
		conf.StopTimeoutSeconds = -1
	case timeout > 0:
		conf.StopTimeoutSeconds = timeout
	}
}

// providerForVM picks the provider from VM state. cniProvider may be nil; bridgeCache must be non-nil.
func providerForVM(conf *config.Config, cniProvider network.Network, bridgeCache map[string]network.Network, vm *types.VM) (network.Network, error) {
	if vm == nil {
		return nil, fmt.Errorf("no VM record")
	}
	if vm.ResolvedNetBackend() == types.BackendBridge {
		dev := vm.ResolvedNetBridgeDev()
		if dev == "" {
			return nil, fmt.Errorf("bridge backend but no bridge device persisted")
		}
		if cached, ok := bridgeCache[dev]; ok {
			return cached, nil
		}
		p, err := cmdcore.InitBridgeNetwork(conf, dev)
		if err != nil {
			return nil, err
		}
		bridgeCache[dev] = p
		return p, nil
	}
	// "cni" or empty (backward compat).
	if cniProvider != nil {
		return cniProvider, nil
	}
	return cmdcore.InitNetwork(conf)
}

// runRoutedLoop runs fn per routed hypervisor, logging each success under logTag (unless JSON), and returns all done ids plus the last error.
func runRoutedLoop(ctx context.Context, logTag, pastTense string, wantJSON bool, routed map[hypervisor.Hypervisor][]string, fn func(hypervisor.Hypervisor, []string) ([]string, error)) (allDone []string, lastErr error) {
	logger := log.WithFunc(logTag)
	for hyper, refs := range routed {
		done, err := fn(hyper, refs)
		if !wantJSON {
			for _, id := range done {
				logger.Infof(ctx, "%s: %s", pastTense, id)
			}
		}
		allDone = append(allDone, done...)
		if err != nil {
			lastErr = err
		}
	}
	return allDone, lastErr
}

// finishRoutedCmd is the shared tail for routed batch commands: wrap lastErr, emit JSON, or log the empty-case message.
func finishRoutedCmd(ctx context.Context, cmd *cobra.Command, logTag, name, pastTense string, allDone []string, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("%s: %w", name, lastErr)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string][]string{"succeeded": allDone}); done {
		return jsonErr
	}
	if len(allDone) == 0 {
		log.WithFunc(logTag).Infof(ctx, "no VMs %s", pastTense)
	}
	return nil
}

func batchRoutedCmd(ctx context.Context, cmd *cobra.Command, name, pastTense string, routed map[hypervisor.Hypervisor][]string, fn func(hypervisor.Hypervisor, []string) ([]string, error)) error {
	logTag := "cmd.vm." + name
	allDone, lastErr := runRoutedLoop(ctx, logTag, pastTense, cliutil.WantJSON(cmd), routed, fn)
	return finishRoutedCmd(ctx, cmd, logTag, name, pastTense, allDone, lastErr)
}

// collectAttachedDevices reads fs/vfio devices; errors are logged and dropped so inspect tolerates a flaky vm.info.
func collectAttachedDevices(ctx context.Context, hyper hypervisor.Hypervisor, ref string) *attachedDevices {
	logger := log.WithFunc("cmd.vm.inspect")
	out := &attachedDevices{}
	if l, ok := hyper.(fs.Lister); ok {
		if devs, err := l.FsList(ctx, ref); err != nil {
			logger.Warnf(ctx, "list fs devices for %s: %v", ref, err)
		} else {
			out.Fs = devs
		}
	}
	if l, ok := hyper.(vfio.Lister); ok {
		if devs, err := l.DeviceList(ctx, ref); err != nil {
			logger.Warnf(ctx, "list vfio devices for %s: %v", ref, err)
		} else {
			out.Devices = devs
		}
	}
	if l, ok := hyper.(disk.Lister); ok {
		if devs, err := l.DiskList(ctx, ref); err != nil {
			logger.Warnf(ctx, "list hot-attached disks for %s: %v", ref, err)
		} else {
			out.Disks = devs
		}
	}
	if len(out.Fs) == 0 && len(out.Devices) == 0 && len(out.Disks) == 0 {
		return nil
	}
	return out
}
