package vm

import (
	"cmp"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

// Hibernate atomically snapshots a running VM and stops it; the snapshot point and the stop coincide, so `vm restore` resumes with nothing lost.
func (h Handler) Hibernate(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.vm.hibernate")
	vmRef := args[0]

	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	hib, ok := hyper.(hypervisor.Hibernator)
	if !ok {
		return fmt.Errorf("backend %s does not support hibernate", hyper.Type())
	}
	// Pin the inspected ID: a same-name delete+recreate between resolve and pause would hibernate the impostor.
	vm, err := hyper.Inspect(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("inspect VM %s: %w", vmRef, err)
	}
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}
	// Fail on a taken name before the VM is even paused.
	name, description, err := cmdcore.SnapshotNameFlags(ctx, cmd, snapBackend)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "hibernating VM %s ...", vmRef)
	// persist runs inside the pause window; the VMM dies only after it succeeds.
	var snapID string
	if err := hib.Hibernate(ctx, vm.ID, func(cfg *types.SnapshotConfig, srcDir string) error {
		id, pErr := cmdcore.PersistSnapshotDir(ctx, snapBackend, cfg, srcDir, name, description)
		snapID = id
		return pErr
	}); err != nil {
		return err
	}
	h.quiesceNetwork(ctx, conf, hyper, []string{vm.ID})
	logger.Infof(ctx, "VM hibernated; snapshot %s (resume: cocoon vm restore %s %s)", snapID, vmRef, cmp.Or(name, snapID))
	return nil
}
