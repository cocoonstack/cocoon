package vm

import (
	"context"
	"errors"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
)

func (h Handler) FsAttach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[fs.Attacher](h, cmd, args, "fs attach", fs.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	socket, _ := cmd.Flags().GetString("socket")
	tag, _ := cmd.Flags().GetString("tag")
	numQ, _ := cmd.Flags().GetInt("num-queues")
	qSize, _ := cmd.Flags().GetInt("queue-size")
	id, err := a.FsAttach(ctx, args[0], fs.Spec{Socket: socket, Tag: tag, NumQueues: numQ, QueueSize: qSize})
	if err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "tag": tag, "id": id}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.fs.attach").Infof(ctx, "attached fs tag=%s id=%s vm=%s", tag, id, args[0])
	return nil
}

func (h Handler) FsDetach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[fs.Attacher](h, cmd, args, "fs detach", fs.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	tag, _ := cmd.Flags().GetString("tag")
	if err := a.FsDetach(ctx, args[0], tag); err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "tag": tag}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.fs.detach").Infof(ctx, "detached fs tag=%s vm=%s", tag, args[0])
	return nil
}

func (h Handler) DeviceAttach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[vfio.Attacher](h, cmd, args, "device attach", vfio.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	pci, _ := cmd.Flags().GetString("pci")
	id, _ := cmd.Flags().GetString("id")
	deviceID, err := a.DeviceAttach(ctx, args[0], vfio.Spec{PCI: pci, ID: id})
	if err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "pci": pci, "id": deviceID}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.device.attach").Infof(ctx, "attached device pci=%s id=%s vm=%s", pci, deviceID, args[0])
	return nil
}

func (h Handler) DeviceDetach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[vfio.Attacher](h, cmd, args, "device detach", vfio.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	id, _ := cmd.Flags().GetString("id")
	if err := a.DeviceDetach(ctx, args[0], id); err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "id": id}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.device.detach").Infof(ctx, "detached device id=%s vm=%s", id, args[0])
	return nil
}

func resolveAttacher[A any](h Handler, cmd *cobra.Command, args []string, op string, errUnsupported error) (context.Context, *config.Config, hypervisor.Hypervisor, A, error) {
	var zero A
	ctx, conf := h.Init(cmd)
	hyper, err := cmdcore.FindHypervisor(ctx, conf, args[0])
	if err != nil {
		return ctx, conf, nil, zero, fmt.Errorf("%s: %w", op, err)
	}
	a, ok := hyper.(A)
	if !ok {
		return ctx, conf, hyper, zero, fmt.Errorf("%s: backend %s: %w", op, hyper.Type(), errUnsupported)
	}
	return ctx, conf, hyper, a, nil
}

func classifyAttachErr(err error) error {
	if errors.Is(err, hypervisor.ErrNotRunning) {
		return fmt.Errorf("vm is not running: %w", err)
	}
	return err
}
