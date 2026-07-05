package vm

import (
	"cmp"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
)

// Hibernate atomically snapshots a running VM and stops it; the snapshot
// point and the stop coincide, so `vm restore` resumes with nothing lost.
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
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	description, _ := cmd.Flags().GetString("description")
	// Fail on a taken name before the VM is stopped, not after.
	if err = cmdcore.EnsureSnapshotNameFree(ctx, snapBackend, name); err != nil {
		return err
	}

	logger.Infof(ctx, "hibernating VM %s ...", vmRef)
	cfg, stream, err := hib.Hibernate(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("hibernate VM %s: %w", vmRef, err)
	}
	defer stream.Close() //nolint:errcheck
	defer cmdcore.CloseOnCancel(ctx, stream)()

	cfg.Name = name
	cfg.Description = description
	snapID, err := snapBackend.Create(ctx, cfg, stream)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	logger.Infof(ctx, "VM hibernated; snapshot %s (resume: cocoon vm restore %s %s)", snapID, vmRef, cmp.Or(name, snapID))
	return nil
}
