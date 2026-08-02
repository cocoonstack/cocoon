package core

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/config"
)

type BaseHandler struct {
	ConfProvider func() *config.Config
}

func (h BaseHandler) Init(cmd *cobra.Command) (context.Context, *config.Config) {
	return cliutil.CommandContext(cmd), h.ConfProvider()
}
