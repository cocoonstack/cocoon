package vm

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) resolveRunningVM(ctx context.Context, conf *config.Config, op, ref string) (*types.VM, error) {
	_, info, err := cmdcore.FindVM(ctx, conf, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if info.State != types.VMStateRunning {
		return nil, fmt.Errorf("%s: %w", op, hypervisor.ErrNotRunning)
	}
	return info, nil
}

func errBackendUnsupported(hyper hypervisor.Hypervisor, op string) error {
	return fmt.Errorf("backend %s does not support %s", hyper.Type(), op)
}

func outputOrLog(cmd *cobra.Command, out any, logFn func()) error {
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, out); done {
		return jsonErr
	}
	logFn()
	return nil
}
