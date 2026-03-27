package others

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/version"
)

type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) GC(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	if err := svc.RunGC(ctx); err != nil {
		return err
	}

	log.WithFunc("cmd.gc").Info(ctx, "GC completed")
	return nil
}

func (h Handler) Version(_ *cobra.Command, _ []string) error {
	fmt.Print(version.String())
	return nil
}
