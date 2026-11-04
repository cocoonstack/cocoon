package vm

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
)

func (h Handler) MigrateCapture(cmd *cobra.Command, args []string) error {
	ctx, _, _, m, err := h.resolveAttacher[hypervisor.Migrator](cmd, args, "migrate capture", hypervisor.ErrMigrateUnsupported)
	if err != nil {
		return err
	}
	req := hypervisor.MigrateRequest{Series: cliutil.FlagStr(cmd, "series"), Final: cliutil.FlagBool(cmd, "final")}
	if req.Final && req.Series == "" {
		return fmt.Errorf("--final needs --series: the baseline is never the last round")
	}
	round, err := m.MigrateCapture(ctx, args[0], req)
	if err != nil {
		return err
	}
	return outputOrLog(cmd, round, func() {
		log.WithFunc("cmd.vm.migrate.capture").Infof(ctx, "series %s round %d in %s (paused: %t, %d files)", round.Series, round.Round, round.Dir, round.Paused, len(round.Files))
	})
}

func (h Handler) MigrateReceive(cmd *cobra.Command, _ []string) error {
	ctx, conf := h.Init(cmd)
	logger := log.WithFunc("cmd.vm.migrate.receive")
	dir := cliutil.FlagStr(cmd, "from-dir")
	cfg, err := readVerifiedEnvelope(dir)
	if err != nil {
		return err
	}
	conf.UseFirecracker = isFirecracker(cfg.Hypervisor)
	hyper, err := cmdcore.InitHypervisor(ctx, conf)
	if err != nil {
		return err
	}
	m, ok := hyper.(hypervisor.Migrator)
	if !ok {
		return fmt.Errorf("migrate receive: backend %s: %w", hyper.Type(), hypervisor.ErrMigrateUnsupported)
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
