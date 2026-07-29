package snapshot

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	"github.com/cocoonstack/cocoon/cmd/cliutil"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
)

// Handler groups the snapshot subcommands.
type Handler struct {
	cmdcore.BaseHandler
}

func (h Handler) Save(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.snapshot.Save")

	vmRef := args[0]
	hyper, err := cmdcore.FindHypervisor(ctx, conf, vmRef)
	if err != nil {
		return fmt.Errorf("find VM %s: %w", vmRef, err)
	}
	// Pin the inspected ID: a same-name delete+recreate between resolve and capture would snapshot the impostor.
	vm, err := hyper.Inspect(ctx, vmRef)
	if err != nil {
		return fmt.Errorf("inspect VM %s: %w", vmRef, err)
	}
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	logger.Infof(ctx, "snapshotting VM %s ...", vmRef)
	snapID, err := cmdcore.CaptureSnapshot(ctx, cmd, snapBackend, func() (*types.SnapshotConfig, string, error) {
		return hyper.Snapshot(ctx, vm.ID)
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
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	vmRef, _ := cmd.Flags().GetString("vm")
	var filterIDs map[string]struct{}
	if vmRef != "" {
		hyper, hyperErr := cmdcore.FindHypervisor(ctx, conf, vmRef)
		if hyperErr != nil {
			return hyperErr
		}
		vm, inspectErr := hyper.Inspect(ctx, vmRef)
		if inspectErr != nil {
			return fmt.Errorf("inspect VM %s: %w", vmRef, inspectErr)
		}
		filterIDs = vm.SnapshotIDs
		if len(filterIDs) == 0 {
			fmt.Println("No snapshots found for VM.")
			return nil
		}
	}

	snapshots, err := snapBackend.List(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	if filterIDs != nil {
		snapshots = slices.DeleteFunc(snapshots, func(s *types.Snapshot) bool {
			_, ok := filterIDs[s.ID]
			return !ok
		})
	}

	if len(snapshots) == 0 {
		fmt.Println("No snapshots found.")
		return nil
	}

	slices.SortFunc(snapshots, func(a, b *types.Snapshot) int { return a.CreatedAt.Compare(b.CreatedAt) })

	return cliutil.OutputFormatted(cmd, snapshots, func(w *tabwriter.Writer) {
		fmt.Fprintln(w, "ID\tNAME\tCPU\tMEMORY\tDESCRIPTION\tCREATED") //nolint:errcheck
		for _, s := range snapshots {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", //nolint:errcheck
				s.ID, s.Name, s.CPU,
				cliutil.FormatSize(s.Memory), s.Description,
				s.CreatedAt.Local().Format(time.DateTime))
		}
	})
}

func (h Handler) Inspect(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	s, err := snapBackend.Inspect(ctx, args[0])
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	return cliutil.OutputJSON(s)
}

func (h Handler) Export(cmd *cobra.Command, args []string) (err error) {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.snapshot.Export")
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	ref := args[0]
	output, _ := cmd.Flags().GetString("output")
	useGzip, _ := cmd.Flags().GetBool("gzip")
	toDir, _ := cmd.Flags().GetString("to-dir")

	if toDir != "" {
		exporter, ok := snapBackend.(snapshot.DirectoryExporter)
		if !ok {
			return fmt.Errorf("backend does not support directory export")
		}
		logger.Infof(ctx, "exporting to dir %s ...", toDir)
		if err = exporter.ExportToDir(ctx, ref, toDir); err != nil {
			return fmt.Errorf("export-to-dir: %w", err)
		}
		logger.Infof(ctx, "exported: %s", toDir)
		return nil
	}

	var stream io.ReadCloser
	if useGzip {
		compressor, ok := snapBackend.(snapshot.CompressedExporter)
		if !ok {
			return fmt.Errorf("backend does not support compressed export")
		}
		stream, err = compressor.ExportCompressed(ctx, ref)
	} else {
		stream, err = snapBackend.Export(ctx, ref)
	}
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer stream.Close() //nolint:errcheck
	defer cmdcore.CloseOnCancel(ctx, stream)()

	if output == "-" {
		if _, err = io.Copy(os.Stdout, stream); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		return nil
	}

	if output == "" {
		snap, inspectErr := snapBackend.Inspect(ctx, ref)
		if inspectErr != nil {
			return fmt.Errorf("inspect: %w", inspectErr)
		}
		base := cmp.Or(snap.Name, snap.ID)
		ext := ".tar"
		if useGzip {
			ext = ".tar.gz"
		}
		output = base + ext
	}

	f, err := os.Create(output) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			os.Remove(output) //nolint:errcheck,gosec
		}
	}()

	logger.Infof(ctx, "exporting to %s ...", output)

	if _, err = io.Copy(f, stream); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}

	logger.Infof(ctx, "exported: %s", output)
	return nil
}

func (h Handler) Import(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.snapshot.Import")
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}

	name, _ := cmd.Flags().GetString("name")
	description, _ := cmd.Flags().GetString("description")

	if err = (&types.SnapshotConfig{Name: name}).Validate(); err != nil {
		return err
	}

	var r io.Reader
	if len(args) > 0 {
		f, openErr := os.Open(args[0]) //nolint:gosec
		if openErr != nil {
			return fmt.Errorf("open archive: %w", openErr)
		}
		defer f.Close() //nolint:errcheck
		r = f
		logger.Infof(ctx, "importing from %s ...", args[0])
	} else {
		r = os.Stdin
		logger.Info(ctx, "importing from stdin ...")
	}

	snapID, err := snapBackend.Import(ctx, r, name, description)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	logger.Infof(ctx, "snapshot imported: %s", snapID)
	return nil
}

func (h Handler) RM(cmd *cobra.Command, args []string) error {
	ctx, conf, err := h.Init(cmd)
	if err != nil {
		return err
	}
	logger := log.WithFunc("cmd.snapshot.RM")
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
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
