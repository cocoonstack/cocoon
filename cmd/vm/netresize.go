package vm

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// netResult is the vm net JSON envelope; Hints carry the guest-side steps a Firecracker PCI VM still needs.
type netResult struct {
	netresize.Result
	Hints []string `json:"hints,omitempty"`
}

// plumbingForVM picks the provider from persisted VM state; 0-NIC works because NetBackend persists.
func plumbingForVM(conf *config.Config, vm *types.VM) (network.Network, error) {
	backend := vm.ResolvedNetBackend()
	if backend == "" {
		return nil, fmt.Errorf("no network backend on VM; cannot resize")
	}
	if backend == types.BackendCNI && vm.ResolvedNetnsPath() == "" {
		return nil, fmt.Errorf("cni backend but no netns; resize would target host netns")
	}
	return cmdcore.NetworkSeam(conf).ForVM(vm)
}

func (h Handler) NetResize(cmd *cobra.Command, args []string) error {
	ctx, conf, hyper, resizer, err := resolveAttacher[netresize.Resizer](h, cmd, args, "vm net", netresize.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	vm, err := hyper.Inspect(ctx, args[0])
	if err != nil {
		return fmt.Errorf("vm net: %w", err)
	}
	plumbing, err := plumbingForVM(conf, vm)
	if err != nil {
		return fmt.Errorf("vm net: %w", err)
	}
	target, _ := cmd.Flags().GetInt("nics")
	res, err := resizer.NetResize(ctx, vm.ID, netresize.Spec{Target: target}, plumbing)
	if err != nil {
		return classifyAttachErr(err)
	}
	out := netResult{Result: res}
	if isFirecracker(hyper.Type()) {
		out.Hints = fcNetHints(res)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, out); done {
		return jsonErr
	}
	logger := log.WithFunc("cmd.vm.net")
	logger.Infof(ctx, "resized %s: before=%d after=%d added=%d removed=%d",
		args[0], res.Before, res.After, len(res.Added), len(res.Removed))
	for _, w := range res.Warnings {
		logger.Warnf(ctx, "%s: %s", args[0], w)
	}
	printGuestHints(out.Hints)
	return nil
}
