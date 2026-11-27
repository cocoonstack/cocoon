package vm

import (
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func (h Handler) Resize(cmd *cobra.Command, args []string) error {
	cpu, memStr := cliutil.FlagInt(cmd, "cpu"), cliutil.FlagStr(cmd, "memory")
	mem, err := types.ParseSize(memStr)
	if err != nil {
		return fmt.Errorf("invalid --memory %q: %w", memStr, err)
	}
	if cpu < 1 || mem < 512<<20 {
		return fmt.Errorf("vm resize: need --cpu >= 1 and --memory >= 512M, got %d and %s", cpu, memStr)
	}
	ctx, _, _, s, err := h.resolveAttacher[hypervisor.Sizer](cmd, args, "vm resize", errors.ErrUnsupported)
	if err != nil {
		return err
	}
	if err := s.SetSize(ctx, args[0], cpu, mem); err != nil {
		return fmt.Errorf("vm resize: %w", err)
	}
	log.WithFunc("cmd.vm.resize").Infof(ctx, "resized vm=%s cpu=%d memory=%d", args[0], cpu, mem)
	return nil
}
