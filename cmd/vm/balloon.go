package vm

import (
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/extend/balloon"
)

type balloonResult struct {
	VM string `json:"vm"`
	balloon.Status
}

func (h Handler) Balloon(cmd *cobra.Command, args []string) error {
	spec, err := balloon.ParseSpec(args[1])
	if err != nil {
		return err
	}
	ctx, _, _, r, err := h.resolveAttacher[balloon.Resizer](cmd, args, "vm balloon", balloon.ErrUnsupported)
	if err != nil {
		return err
	}
	st, err := r.BalloonResize(ctx, args[0], spec)
	if err != nil {
		return err
	}
	return outputOrLog(cmd, balloonResult{VM: args[0], Status: st}, func() {
		log.WithFunc("cmd.vm.balloon").Infof(ctx, "set balloon target vm=%s target=%d default=%d", args[0], st.Target, st.Default)
	})
}
