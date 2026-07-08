package vm

import (
	"bytes"
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
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
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

	force, _ := cmd.Flags().GetBool("force")
	timeout, _ := cmd.Flags().GetInt("timeout")

	if force {
		conf.StopTimeoutSeconds = -1
	} else if timeout > 0 {
		conf.StopTimeoutSeconds = timeout
	}

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}
	return batchRoutedCmd(ctx, cmd, "stop", "stopped", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Stop(ctx, refs)
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
	// force means "now", docker-rm-like: skip the graceful window the same
	// way stop --force does — FC guests without i8042 never see CtrlAltDel
	// and would burn the full 30s per VM otherwise (#82).
	if force {
		conf.StopTimeoutSeconds = -1
	}

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	const logTag = "cmd.vm.rm"
	allDeleted, lastErr := runRoutedLoop(ctx, logTag, "deleted", cliutil.WantJSON(cmd), routed,
		func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
			return hyper.Delete(ctx, refs, force)
		})

	if len(allDeleted) > 0 {
		if netProvider, initErr := cmdcore.InitNetwork(conf); initErr == nil {
			if _, delErr := netProvider.Delete(ctx, allDeleted); delErr != nil {
				return fmt.Errorf("vm(s) deleted but network cleanup failed: %w", delErr)
			}
		}
		bridgenet.CleanupTAPs(allDeleted)
	}

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
		if netProvider.Verify(ctx, vm.ID) == nil {
			continue
		}
		logger.Warnf(ctx, "network missing for VM %s, recovering", vm.ID)
		if _, prepErr := netProvider.Prepare(ctx, vm.ID, &vm.Config); prepErr != nil {
			logger.Warnf(ctx, "prepare netns for VM %s: %v (start will fail)", vm.ID, prepErr)
			continue
		}
		if len(vm.NetworkConfigs) == 0 {
			continue
		}
		if _, recoverErr := netProvider.Add(ctx, vm.ID, &vm.Config, network.AddRecover(vm.NetworkConfigs)...); recoverErr != nil {
			logger.Warnf(ctx, "recover network for VM %s: %v (start will fail)", vm.ID, recoverErr)
		}
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
	if len(out.Fs) == 0 && len(out.Devices) == 0 {
		return nil
	}
	return out
}

func streamLog(ctx context.Context, path string, follow bool, tail int) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("open log %s: VM may not have been started yet", path)
		}
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if tail > 0 {
		if seekErr := seekToLastNLines(f, tail); seekErr != nil {
			return fmt.Errorf("seek tail: %w", seekErr)
		}
	}

	if !follow {
		if _, copyErr := io.Copy(os.Stdout, f); copyErr != nil {
			return fmt.Errorf("read log: %w", copyErr)
		}
		return nil
	}

	events, err := utils.WatchFile(ctx, path, logFollowDebounce)
	if err != nil {
		return fmt.Errorf("watch log: %w", err)
	}
	sig, _ := utils.FileHead(f, logHeadSigLen)
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			// Sig mismatch catches O_TRUNC re-opens (CH/FC stamp a unique boot timestamp on line 1).
			newSig, _ := utils.FileHead(f, logHeadSigLen)
			if !bytes.Equal(newSig, sig) {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("rewind log: %w", err)
				}
				sig = newSig
			}
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return fmt.Errorf("read log: %w", err)
			}
		}
	}
}

// seekToLastNLines positions f so a subsequent read returns the last n lines. A final trailing '\n' is not counted as a line separator.
func seekToLastNLines(f *os.File, n int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	const chunk = 4096
	buf := make([]byte, chunk)
	pos, found := size, 0
	for pos > 0 {
		readSize := min(int64(chunk), pos)
		pos -= readSize
		if _, readErr := f.ReadAt(buf[:readSize], pos); readErr != nil {
			return readErr
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] != '\n' || pos+i == size-1 {
				continue
			}
			found++
			if found == n {
				_, seekErr := f.Seek(pos+i+1, io.SeekStart)
				return seekErr
			}
		}
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}
