package vm

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

func (h Handler) MigrateCapture(cmd *cobra.Command, args []string) error {
	req := hypervisor.MigrateRequest{Series: cliutil.FlagStr(cmd, "series"), Final: cliutil.FlagBool(cmd, "final")}
	if req.Final && req.Series == "" {
		return fmt.Errorf("--final needs --series: the baseline is never the last round")
	}
	ctx, _, _, m, err := h.resolveAttacher[hypervisor.Migrator](cmd, args, "migrate capture", hypervisor.ErrMigrateUnsupported)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.vm.migrate.capture")
	round, err := m.MigrateCapture(ctx, args[0], req)
	if err != nil {
		return err
	}
	return outputOrLog(cmd, round, func() {
		logger.Infof(ctx, "series %s round %d in %s (paused: %t, %d files)", round.Series, round.Round, round.Dir, round.Paused, len(round.Files))
	})
}

func (h Handler) MigrateReceive(cmd *cobra.Command, _ []string) error {
	id := cliutil.FlagStr(cmd, "id")
	if id != "" && !utils.ValidID(id) {
		return fmt.Errorf("--id must have the shape of a generated VM ID, got %q", id)
	}
	ctx, conf := h.Init(cmd)
	logger := log.WithFunc("cmd.vm.migrate.receive")
	dir := cliutil.FlagStr(cmd, "from-dir")
	cfg, hyper, err := openSnapshotDir(ctx, conf, dir)
	if err != nil {
		return err
	}
	m, ok := hyper.(hypervisor.Migrator)
	if !ok {
		return fmt.Errorf("migrate receive: backend %s: %w", hyper.Type(), hypervisor.ErrMigrateUnsupported)
	}
	// A receive killed mid-flight leaves its placeholder and maybe a VMM under this ID.
	if id != "" {
		if _, err = hyper.ReconcileStaleCreate(ctx, id); err != nil {
			return fmt.Errorf("reclaim interrupted receive: %w", err)
		}
	}
	cs, err := h.prepareClone(ctx, cmd, conf, hyper, cfg)
	if err != nil {
		return err
	}
	defer cs.unlock()
	vm, err := m.MigrateReceive(ctx, cs.vmID, cs.vmCfg, cs.netSetup, &cfg, dir)
	if err != nil {
		rollbackNetwork(ctx, cs.netProvider, cs.vmID)
		cs.rollback()
		return fmt.Errorf("migrate receive: %w", err)
	}
	return outputOrLog(cmd, vm, func() {
		logger.Infof(ctx, "VM received: %s (name: %s), guest paused; resume with: cocoon vm start %s", vm.ID, vm.Config.Name, vm.ID)
	})
}

func (h Handler) MigrateAbort(cmd *cobra.Command, args []string) error {
	ctx, _, _, m, err := h.resolveAttacher[hypervisor.Migrator](cmd, args, "migrate abort", hypervisor.ErrMigrateUnsupported)
	if err != nil {
		return err
	}
	return m.MigrateAbort(ctx, args[0], cliutil.FlagStr(cmd, "series"))
}
