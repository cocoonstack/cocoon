package vm

import (
	"context"
	"fmt"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

type Handler struct {
	cmdcore.BaseHandler
}

// resolveRunningVM finds ref's hypervisor, inspects it, and requires a running VM.
// op prefixes the wrapped errors (e.g. "exec", "reseed").
func (h Handler) resolveRunningVM(ctx context.Context, conf *config.Config, op, ref string) (*types.VM, error) {
	hyper, err := cmdcore.FindHypervisor(ctx, conf, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	info, err := hyper.Inspect(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect: %w", op, err)
	}
	if info.State != types.VMStateRunning {
		return nil, fmt.Errorf("%s: %w", op, hypervisor.ErrNotRunning)
	}
	return info, nil
}
