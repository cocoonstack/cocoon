package meta

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
)

func TestConvertRejectsPinnedScope(t *testing.T) {
	conf := &config.Config{PinHypervisor: true, MetaBackend: "sqlite"}
	h := Handler{BaseHandler: cmdcore.BaseHandler{ConfProvider: func() *config.Config { return conf }}}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	if err := h.Convert(cmd, nil); err == nil || !strings.Contains(err.Error(), "whole root") {
		t.Fatalf("Convert error = %v, want whole-root pin rejection", err)
	}
	if err := h.InitStore(cmd, nil); err == nil || !strings.Contains(err.Error(), "whole root") {
		t.Fatalf("InitStore error = %v, want whole-root pin rejection", err)
	}
}
