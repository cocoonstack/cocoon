package vm

import (
	"context"
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
)

type staleCreateResult struct {
	ID      string                        `json:"id,omitempty"`
	Outcome hypervisor.StaleCreateOutcome `json:"outcome"`
}

func (h Handler) ReconcileStaleCreate(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	hyper, vm, err := cmdcore.FindVM(ctx, conf, args[0])
	if errors.Is(err, hypervisor.ErrNotFound) {
		return outputStaleCreate(ctx, cmd, args[0], staleCreateResult{Outcome: hypervisor.StaleCreateNotFound})
	}
	if err != nil {
		return err
	}
	sup, ok := hyper.(hypervisor.Supervisable)
	if !ok {
		return fmt.Errorf("backend %s cannot reconcile stale creates", hyper.Type())
	}
	outcome, err := sup.ReconcileStaleCreate(ctx, vm.ID)
	if err != nil {
		return fmt.Errorf("reconcile stale create: %w", err)
	}
	return outputStaleCreate(ctx, cmd, args[0], staleCreateResult{ID: vm.ID, Outcome: outcome})
}

func outputStaleCreate(ctx context.Context, cmd *cobra.Command, ref string, res staleCreateResult) error {
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, res); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.reconcileStaleCreate").Infof(ctx, "%s: %s", ref, res.Outcome)
	return nil
}
