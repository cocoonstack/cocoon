package core

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/config"
)

type BaseHandler struct {
	ConfProvider func() *config.Config
}

func (h BaseHandler) Init(cmd *cobra.Command) (context.Context, *config.Config, error) {
	conf, err := h.Conf()
	if err != nil {
		return nil, nil, err
	}
	return CommandContext(cmd), conf, nil
}

func (h BaseHandler) Conf() (*config.Config, error) {
	if h.ConfProvider == nil {
		return nil, fmt.Errorf("config provider is nil")
	}
	conf := h.ConfProvider()
	if conf == nil {
		return nil, fmt.Errorf("config not initialized")
	}
	return conf, nil
}

func CommandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}
