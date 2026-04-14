package others

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/network/bridge"
	"github.com/cocoonstack/cocoon/version"
)

// Handler groups miscellaneous CLI commands (gc, version).
type Handler struct {
	cmdcore.BaseHandler
}

// GC handles the 'gc' command.
func (h Handler) GC(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	backends, err := cmdcore.InitImageBackends(ctx, conf)
	if err != nil {
		return err
	}
	netProvider, err := cmdcore.InitNetwork(conf)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	o := gc.New()
	for _, b := range backends {
		b.RegisterGC(o)
	}
	// Register ALL hypervisor backends so GC protects blobs from both CH and FC VMs.
	hypers, hyperErr := cmdcore.InitAllHypervisors(conf)
	if hyperErr != nil {
		return hyperErr
	}
	for _, hyper := range hypers {
		hyper.RegisterGC(o)
	}
	netProvider.RegisterGC(o)
	gc.Register(o, bridge.GCModule(conf.RootDir))
	snapBackend.RegisterGC(o)
	if err := o.Run(ctx); err != nil {
		return err
	}
	log.WithFunc("cmd.gc").Info(ctx, "GC completed")
	return nil
}

// Version handles the 'version' command.
func (h Handler) Version(_ *cobra.Command, _ []string) error {
	fmt.Print(version.String())
	return nil
}
