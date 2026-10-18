package vm

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
)

type diskResult struct {
	VM     string   `json:"vm"`
	Name   string   `json:"name"`
	ID     string   `json:"id,omitempty"`
	Format string   `json:"format,omitempty"`
	Hints  []string `json:"hints,omitempty"`
}

func (h Handler) DiskAttach(cmd *cobra.Command, args []string) error {
	fromSnapshot, _ := cmd.Flags().GetString("from-snapshot")
	fromDir, _ := cmd.Flags().GetString("from-dir")
	if fromSnapshot != "" || fromDir != "" {
		return h.diskAttachSnapshot(cmd, args, fromSnapshot, fromDir)
	}
	ctx, _, hyper, a, err := h.resolveAttacher[disk.Attacher](cmd, args, "disk attach", disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	path, _ := cmd.Flags().GetString("path")
	name, _ := cmd.Flags().GetString("name")
	readonly, _ := cmd.Flags().GetBool("readonly")
	dioVal, _ := cmd.Flags().GetString("directio")
	directIO, err := types.ParseDirectIO(dioVal)
	if err != nil {
		return err
	}
	id, err := a.DiskAttach(ctx, args[0], disk.Spec{Path: path, Name: name, ReadOnly: readonly, DirectIO: directIO})
	if err != nil {
		return classifyAttachErr(err)
	}
	out := diskResult{VM: args[0], Name: name, ID: id}
	if isFirecracker(hyper.Type()) {
		out.Hints = []string{pciRescanHint}
	}
	return outputOrLog(cmd, out, func() {
		log.WithFunc("cmd.vm.disk.attach").Infof(ctx, "attached disk name=%s id=%s vm=%s", name, id, args[0])
		printGuestHints(out.Hints)
	})
}

func (h Handler) DiskDetach(cmd *cobra.Command, args []string) error {
	ctx, _, hyper, a, err := h.resolveAttacher[disk.Attacher](cmd, args, "disk detach", disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	if err := a.DiskDetach(ctx, args[0], name); err != nil {
		return classifyAttachErr(err)
	}
	out := diskResult{VM: args[0], Name: name}
	if isFirecracker(hyper.Type()) {
		out.Hints = []string{pciDiskRemoveHint}
	}
	return outputOrLog(cmd, out, func() {
		log.WithFunc("cmd.vm.disk.detach").Infof(ctx, "detached disk name=%s vm=%s", name, args[0])
		printGuestHints(out.Hints)
	})
}

func (h Handler) diskAttachSnapshot(cmd *cobra.Command, args []string, fromSnapshot, fromDir string) error {
	ctx, conf, _, a, err := h.resolveAttacher[disk.SnapshotAttacher](cmd, args, "snapshot disk attach", disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	dir, cfg, release, err := openSnapshotSource(ctx, conf, fromSnapshot, fromDir)
	if err != nil {
		return err
	}
	defer release()
	basePath, err := cloudimgBasePath(ctx, conf, cfg.ImageBlobIDs)
	if err != nil {
		return err
	}
	name := disk.SnapshotName(cfg.ID)
	id, format, err := a.DiskAttachSnapshot(ctx, args[0], disk.SnapshotSource{Dir: dir, Name: name, BlobIDs: cfg.ImageBlobIDs, BasePath: basePath})
	if err != nil {
		return classifyAttachErr(err)
	}
	return outputOrLog(cmd, diskResult{VM: args[0], Name: name, ID: id, Format: format}, func() {
		log.WithFunc("cmd.vm.disk.attach").Infof(ctx, "attached snapshot disk name=%s id=%s format=%s vm=%s", name, id, format, args[0])
	})
}

// openSnapshotSource holds a --from-snapshot read lease until release, so delete/GC cannot reap the dir mid-copy.
func openSnapshotSource(ctx context.Context, conf *config.Config, fromSnapshot, fromDir string) (string, types.SnapshotConfig, func(), error) {
	if fromDir != "" {
		envelope, err := snapshot.ReadSnapshotEnvelope(fromDir)
		if err != nil {
			return "", types.SnapshotConfig{}, nil, fmt.Errorf("load envelope: %w", err)
		}
		if err = snapshot.VerifyCOWSize(fromDir, envelope.Config); err != nil {
			return "", types.SnapshotConfig{}, nil, err
		}
		return fromDir, envelope.Config, func() {}, nil
	}
	snapBackend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return "", types.SnapshotConfig{}, nil, err
	}
	da, ok := snapBackend.(snapshot.Direct)
	if !ok {
		return "", types.SnapshotConfig{}, nil, fmt.Errorf("snapshot backend %s has no local data dir", snapBackend.Type())
	}
	dir, cfg, release, err := da.DataDir(ctx, fromSnapshot)
	if err != nil {
		return "", types.SnapshotConfig{}, nil, fmt.Errorf("open snapshot %s: %w", fromSnapshot, err)
	}
	return dir, cfg, release, nil
}

func cloudimgBasePath(ctx context.Context, conf *config.Config, blobIDs map[string]struct{}) (string, error) {
	_, cloudimgStore, err := cmdcore.InitImageBackendsForPull(ctx, conf)
	if err != nil {
		return "", err
	}
	for hex := range blobIDs {
		if cloudimgStore.OwnsBlob(hex) {
			return cloudimgStore.BlobPath(hex), nil
		}
	}
	return "", nil
}
