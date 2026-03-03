package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/projecteru2/cocoon/cmd/core"
	"github.com/projecteru2/cocoon/snapshot"
	"github.com/projecteru2/cocoon/types"
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
	logger := log.WithFunc("cmd.snapshot.save")

	hyper, err := cmdcore.InitHypervisor(conf)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	vmRef := args[0]
	name, _ := cmd.Flags().GetString("name")
	description, _ := cmd.Flags().GetString("description")

	// Pre-check: reject if the snapshot name is already taken.
	if name != "" {
		if _, inspectErr := snapBackend.Inspect(ctx, name); inspectErr == nil {
			return fmt.Errorf("snapshot name %q already exists", name)
		} else if !errors.Is(inspectErr, snapshot.ErrNotFound) {
			return fmt.Errorf("check snapshot name: %w", inspectErr)
		}
	}

	logger.Infof(ctx, "snapshotting VM %s ...", vmRef)

	cfg, stream, err := hyper.Snapshot(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("snapshot VM %s: %w", vmRef, err)
	}
	defer stream.Close() //nolint:errcheck

	// Close stream on context cancellation to unblock the pipe immediately,
	// so Ctrl+C doesn't hang while streaming large snapshot data.
	stop := context.AfterFunc(ctx, func() {
		stream.Close() //nolint:errcheck,gosec
	})
	defer stop()

	cfg.Name = name
	cfg.Description = description

	logger.Info(ctx, "saving snapshot data ...")

	snapID, err := snapBackend.Create(ctx, cfg, stream)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}

	logger.Infof(ctx, "snapshot saved: %s", snapID)
	return nil
}

func (h Handler) List(cmd *cobra.Command, _ []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	snapshots, err := snapBackend.List(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(snapshots) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	slices.SortFunc(snapshots, func(a, b *types.Snapshot) int { return a.CreatedAt.Compare(b.CreatedAt) })

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snapshots)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tCPU\tMEMORY\tDESCRIPTION\tCREATED")
	for _, s := range snapshots {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			s.ID,
			s.Name,
			s.CPU,
			cmdcore.FormatSize(s.Memory),
			s.Description,
			s.CreatedAt.Local().Format(time.DateTime),
		)
	}
	w.Flush() //nolint:errcheck,gosec
	return nil
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	s, err := snapBackend.Inspect(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.snapshot.rm")
	snapBackend, err := cmdcore.InitSnapshot(conf)
	if err != nil {
		return err
	}

	deleted, err := snapBackend.Delete(ctx, args)
	for _, id := range deleted {
		logger.Infof(ctx, "deleted: %s", id)
	}
	if err != nil {
		return fmt.Errorf("rm: %w", err)
	}
	if len(deleted) == 0 {
		logger.Info(ctx, "no snapshots deleted")
	}
	return nil
}
