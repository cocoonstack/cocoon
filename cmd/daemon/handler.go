package daemon

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	cocoond "github.com/cocoonstack/cocoon/daemon"
)

const apiSockName = "cocoond.sock"

// Handler runs the resident supervisor.
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Run(cmd *cobra.Command, _ []string) error {
	ctx, conf := h.Init(cmd)
	store, err := cmdcore.MetaStore(conf)
	if err != nil {
		return err
	}
	hypers, err := cmdcore.InitAllHypervisors(ctx, conf)
	if err != nil {
		return err
	}
	supervisors := make([]cocoond.Supervisor, 0, len(hypers))
	for _, hyper := range hypers {
		s, ok := hyper.(cocoond.Supervisor)
		if !ok {
			return fmt.Errorf("hypervisor %s cannot be supervised", hyper.Type())
		}
		supervisors = append(supervisors, s)
	}
	dconf, err := daemonConfig(cmd, conf)
	if err != nil {
		return err
	}
	d, err := cocoond.New(dconf, store, supervisors)
	if err != nil {
		return err
	}
	return d.Run(ctx)
}

func daemonConfig(cmd *cobra.Command, conf *config.Config) (cocoond.Config, error) {
	interval, _ := cmd.Flags().GetDuration("reconcile-interval")
	if interval < 0 {
		return cocoond.Config{}, fmt.Errorf("--reconcile-interval must be >= 0, got %s", interval)
	}
	gcInterval, _ := cmd.Flags().GetDuration("gc-interval")
	if gcInterval < 0 {
		return cocoond.Config{}, fmt.Errorf("--gc-interval must be >= 0, got %s", gcInterval)
	}
	sockMode, _ := cmd.Flags().GetUint32("api-socket-mode")

	addr, _ := cmd.Flags().GetString("api-socket")
	addr = cmp.Or(addr, filepath.Join(conf.RunDir, apiSockName))
	if noAPI, _ := cmd.Flags().GetBool("no-api"); noAPI {
		addr = ""
	}

	return cocoond.Config{
		RootDir:           conf.RootDir,
		ReconcileInterval: interval,
		GCInterval:        gcInterval,
		GC:                gcRunner(conf, gcInterval),
		APIAddr:           addr,
		APISockMode:       sockMode,
	}, nil
}

// gcRunner builds the orchestrator per run so each sweep reads fresh state.
func gcRunner(conf *config.Config, interval time.Duration) func(context.Context) error {
	if interval <= 0 {
		return nil
	}
	return func(ctx context.Context) error {
		o, err := cmdcore.NewGCOrchestrator(ctx, conf)
		if err != nil {
			return err
		}
		return o.Run(ctx)
	}
}
