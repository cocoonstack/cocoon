package vm

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/cocoonstack/cocoon/types"
)

const (
	// logHeadSigLen spans CH/FC's boot timestamp on line 1.
	logHeadSigLen = 64

	logFollowDebounce = 100 * time.Millisecond
)

type batchOp func(hypervisor.Hypervisor, []string) ([]string, error)

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
	ctx, conf := h.Init(cmd)

	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	return batchRoutedCmd(ctx, cmd, "start", "started", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Start(ctx, refs)
	})
}

func (h Handler) Stop(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)

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
		return hyper.Stop(ctx, refs)
	})
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)

	hyper, info, err := cmdcore.FindVM(ctx, conf, args[0])
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
	ctx, conf := h.Init(cmd)

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

	if f, ok := conn.(*os.File); ok {
		cleanup := console.HandleResize(inFd, f.Fd())
		defer cleanup()
	}

	escapeKeys := []byte{escapeChar, '.'}
	if err := console.Relay(conn, escapeKeys); err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}

func (h Handler) Logs(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
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
	ctx, conf := h.Init(cmd)
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

	return batchRoutedCmd(ctx, cmd, "rm", "deleted", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Delete(ctx, refs, force)
	})
}

// applyStopFlags maps --force to an immediate kill: #82 FC guests without i8042 never answer CtrlAltDel.
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

func batchRoutedCmd(ctx context.Context, cmd *cobra.Command, name, pastTense string, routed map[hypervisor.Hypervisor][]string, fn batchOp) error {
	logger := log.WithFunc("cmd.vm." + name)
	wantJSON := cliutil.WantJSON(cmd)
	var allDone []string
	var errs []error
	for hyper, refs := range routed {
		done, err := fn(hyper, refs)
		if !wantJSON {
			for _, id := range done {
				logger.Infof(ctx, "%s: %s", pastTense, id)
			}
		}
		allDone = append(allDone, done...)
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string][]string{"succeeded": allDone}); done {
		return jsonErr
	}
	if len(allDone) == 0 {
		logger.Infof(ctx, "no VMs %s", pastTense)
	}
	return nil
}

// collectAttachedDevices logs and drops errors so inspect tolerates a flaky vm.info.
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
