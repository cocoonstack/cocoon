package vm

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/moby/term"
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/console"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	bridgenet "github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/types"
)

// Start handles the 'vm start' command.
func (h Handler) Start(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	hypers, err := cmdcore.InitAllHypervisors(conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	// Recover network for all backends before starting.
	for hyper, refs := range routed {
		h.recoverNetwork(ctx, conf, hyper, refs)
	}

	return batchRoutedCmd(ctx, "start", "started", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Start(ctx, refs)
	})
}

// Stop handles the 'vm stop' command.
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

	hypers, err := cmdcore.InitAllHypervisors(conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}
	return batchRoutedCmd(ctx, "stop", "stopped", routed, func(hyper hypervisor.Hypervisor, refs []string) ([]string, error) {
		return hyper.Stop(ctx, refs)
	})
}

// Inspect handles the 'vm inspect' command.
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
	return cmdcore.OutputJSON(info)
}

// Console handles the 'vm console' command.
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

// RM handles the 'vm rm' command.
func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.rm")

	force, _ := cmd.Flags().GetBool("force")

	hypers, err := cmdcore.InitAllHypervisors(conf)
	if err != nil {
		return err
	}
	routed, err := cmdcore.RouteRefs(ctx, hypers, args)
	if err != nil {
		return err
	}

	var allDeleted []string
	var lastErr error
	for hyper, refs := range routed {
		deleted, deleteErr := hyper.Delete(ctx, refs, force)
		for _, id := range deleted {
			logger.Infof(ctx, "deleted VM: %s", id)
		}
		allDeleted = append(allDeleted, deleted...)
		if deleteErr != nil {
			lastErr = deleteErr
		}
	}

	if len(allDeleted) > 0 {
		if netProvider, initErr := cmdcore.InitNetwork(conf); initErr == nil {
			if _, delErr := netProvider.Delete(ctx, allDeleted); delErr != nil {
				return fmt.Errorf("vm(s) deleted but network cleanup failed: %w", delErr)
			}
		}
		// Also clean up bridge TAPs (no-op if none exist).
		bridgenet.CleanupTAPs(allDeleted)
	}

	if lastErr != nil {
		return fmt.Errorf("rm: %w", lastErr)
	}
	if len(allDeleted) == 0 {
		logger.Info(ctx, "no VMs deleted")
	}
	return nil
}

func (h Handler) recoverNetwork(ctx context.Context, conf *config.Config, hyper hypervisor.Hypervisor, refs []string) {
	logger := log.WithFunc("cmd.recoverNetwork")

	// Lazy-init CNI provider (may fail if not configured — OK for bridge-only setups).
	var cniProvider network.Network
	if p, err := cmdcore.InitNetwork(conf); err == nil {
		cniProvider = p
	}

	// Cache bridge providers by device name to avoid redundant netlink lookups.
	bridgeProviders := map[string]network.Network{}

	for _, ref := range refs {
		vm, err := hyper.Inspect(ctx, ref)
		if err != nil || vm == nil || len(vm.NetworkConfigs) == 0 {
			continue
		}

		netProvider, provErr := providerForVM(conf, cniProvider, bridgeProviders, vm.NetworkConfigs)
		if provErr != nil {
			logger.Warnf(ctx, "skip recovery for VM %s: %v", vm.ID, provErr)
			continue
		}
		if netProvider.Verify(ctx, vm.ID) == nil {
			continue
		}
		logger.Warnf(ctx, "network missing for VM %s, recovering", vm.ID)
		if _, recoverErr := netProvider.Config(ctx, vm.ID, len(vm.NetworkConfigs), &vm.Config, vm.NetworkConfigs...); recoverErr != nil {
			logger.Warnf(ctx, "recover network for VM %s: %v (start will fail)", vm.ID, recoverErr)
		}
	}
}

// providerForVM selects the correct network provider based on persisted NetworkConfig.
func providerForVM(conf *config.Config, cniProvider network.Network, bridgeCache map[string]network.Network, configs []*types.NetworkConfig) (network.Network, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("no network configs")
	}
	// All NICs on a VM share the same backend.
	cfg := configs[0]
	if cfg.Backend == "bridge" {
		if cfg.BridgeDev == "" {
			return nil, fmt.Errorf("bridge backend but no bridge device persisted")
		}
		if cached, ok := bridgeCache[cfg.BridgeDev]; ok {
			return cached, nil
		}
		p, err := cmdcore.InitBridgeNetwork(conf, cfg.BridgeDev)
		if err != nil {
			return nil, err
		}
		bridgeCache[cfg.BridgeDev] = p
		return p, nil
	}
	// "cni" or empty (backward compat).
	if cniProvider == nil {
		return nil, fmt.Errorf("CNI provider not available")
	}
	return cniProvider, nil
}

// batchRoutedCmd runs a batch operation across multiple backends.
func batchRoutedCmd(ctx context.Context, name, pastTense string, routed map[hypervisor.Hypervisor][]string, fn func(hypervisor.Hypervisor, []string) ([]string, error)) error {
	logger := log.WithFunc("cmd." + name)
	var allDone []string
	var lastErr error
	for hyper, refs := range routed {
		done, err := fn(hyper, refs)
		for _, id := range done {
			logger.Infof(ctx, "%s: %s", pastTense, id)
		}
		allDone = append(allDone, done...)
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("%s: %w", name, lastErr)
	}
	if len(allDone) == 0 {
		logger.Infof(ctx, "no VMs %s", pastTense)
	}
	return nil
}
