package vm

import (
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func (h Handler) Resize(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	logger := log.WithFunc("cmd.vm.resize")
	spec, err := resizeSpecFromFlags(cmd)
	if err != nil {
		return err
	}
	hyper, before, err := cmdcore.FindVM(ctx, conf, args[0])
	if err != nil {
		return fmt.Errorf("vm resize: %w", err)
	}
	vm, err := hyper.Resize(ctx, before.ID, spec)
	if err != nil {
		return fmt.Errorf("vm resize: %w", err)
	}
	logger.Infof(ctx, "resized vm=%s cpu=%d memory=%s storage=%s; the next vm start cold-boots at this size",
		args[0], vm.Config.CPU, cliutil.FormatSize(vm.Config.Memory), cliutil.FormatSize(vm.Config.Storage))
	if vm.Config.Storage > before.Config.Storage && vm.Config.ImageType == types.ImageTypeCloudImg {
		logger.Warnf(ctx, "%s boots a cloud image: grow the partition and filesystem inside the guest, e.g. growpart /dev/vda 1 && resize2fs /dev/vda1", args[0])
	}
	return nil
}

func resizeSpecFromFlags(cmd *cobra.Command) (hypervisor.ResizeSpec, error) {
	spec := hypervisor.ResizeSpec{CPU: cliutil.FlagInt(cmd, "cpu")}
	var err error
	if spec.Memory, err = cliutil.FlagSize(cmd, "memory"); err != nil {
		return spec, err
	}
	if spec.Storage, err = cliutil.FlagSize(cmd, "storage"); err != nil {
		return spec, err
	}
	for name, zero := range map[string]bool{"cpu": spec.CPU == 0, "memory": spec.Memory == 0, "storage": spec.Storage == 0} {
		if zero && cmd.Flags().Changed(name) {
			return spec, fmt.Errorf("--%s must be above zero; leave the flag out to keep the current value", name)
		}
	}
	return spec, nil
}
