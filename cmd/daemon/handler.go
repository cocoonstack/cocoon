package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	cocoond "github.com/cocoonstack/cocoon/daemon"
)

// apiSockName is the read-only API socket's default name under the run dir.
const apiSockName = "cocoond.sock"

// Handler runs the resident supervisor.
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Run(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
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
	if noAPI, _ := cmd.Flags().GetBool("no-api"); noAPI {
		addr = ""
	} else if addr == "" {
		addr = filepath.Join(conf.RunDir, apiSockName)
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

// gcRunner builds the orchestrator per run so a sweep always reads fresh state; nil keeps GC a CLI verb.
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
