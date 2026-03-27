package snapshot

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/service"
)

// Handler implements Actions.
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Save(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.snapshot.save")
	vmRef := args[0]
	name, _ := cmd.Flags().GetString("name")
	description, _ := cmd.Flags().GetString("description")

	logger.Infof(ctx, "snapshotting VM %s ...", vmRef)

	snapID, err := svc.SaveSnapshot(ctx, &service.SnapshotSaveParams{
		VMRef:       vmRef,
		Name:        name,
		Description: description,
	})
	if err != nil {
		return err
	}

	logger.Infof(ctx, "snapshot saved: %s", snapID)
	return nil
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	vmRef, _ := cmd.Flags().GetString("vm")

	snapshots, err := svc.ListSnapshots(ctx, vmRef)
	if err != nil {
		return err
	}

	if len(snapshots) == 0 {
		if vmRef != "" {
			fmt.Println("No snapshots found for VM.")
		} else {
			fmt.Println("No snapshots found.")
		}
		return nil
	}

	return cmdcore.OutputFormatted(cmd, snapshots, func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "ID\tNAME\tCPU\tMEMORY\tDESCRIPTION\tCREATED") //nolint:errcheck
		for _, s := range snapshots {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", //nolint:errcheck
				s.ID, s.Name, s.CPU,
				cmdcore.FormatSize(s.Memory), s.Description,
				s.CreatedAt.Local().Format(time.DateTime))
		}
	})
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	s, err := svc.InspectSnapshot(ctx, args[0])
	if err != nil {
		return err
	}

	return cmdcore.OutputJSON(s)
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}

	svc, err := cmdcore.InitService(cmd, conf)
	if err != nil {
		return err
	}

	logger := log.WithFunc("cmd.snapshot.rm")

	deleted, err := svc.RemoveSnapshots(ctx, args)
	for _, id := range deleted {
		logger.Infof(ctx, "deleted: %s", id)
	}

	if err != nil {
		return err
	}

	if len(deleted) == 0 {
		logger.Info(ctx, "no snapshots deleted")
	}

	return nil
}
