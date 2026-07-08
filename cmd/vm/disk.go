package vm

import (
	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/extend/disk"
)

func (h Handler) DiskAttach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[disk.Attacher](h, cmd, args, "disk attach", disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	path, _ := cmd.Flags().GetString("path")
	name, _ := cmd.Flags().GetString("name")
	readonly, _ := cmd.Flags().GetBool("readonly")
	dioVal, _ := cmd.Flags().GetString("directio")
	directIO, err := cmdcore.ParseDirectIO(dioVal)
	if err != nil {
		return err
	}
	id, err := a.DiskAttach(ctx, args[0], disk.Spec{Path: path, Name: name, ReadOnly: readonly, DirectIO: directIO})
	if err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "name": name, "id": id}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.disk.attach").Infof(ctx, "attached disk name=%s id=%s vm=%s", name, id, args[0])
	return nil
}

func (h Handler) DiskDetach(cmd *cobra.Command, args []string) error {
	ctx, _, _, a, err := resolveAttacher[disk.Attacher](h, cmd, args, "disk detach", disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	if err := a.DiskDetach(ctx, args[0], name); err != nil {
		return classifyAttachErr(err)
	}
	if done, jsonErr := cliutil.MaybeOutputJSON(cmd, map[string]string{"vm": args[0], "name": name}); done {
		return jsonErr
	}
	log.WithFunc("cmd.vm.disk.detach").Infof(ctx, "detached disk name=%s vm=%s", name, args[0])
	return nil
}
