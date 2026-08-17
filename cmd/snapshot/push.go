package snapshot

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/projecteru2/core/log"
	"github.com/spf13/cobra"

	commonsnapshot "github.com/cocoonstack/cocoon-common/snapshot"
	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	localsnapshot "github.com/cocoonstack/cocoon/snapshot"
)

type backendExporter struct {
	backend localsnapshot.Snapshot
	ref     string
}

type backendExportStream struct {
	io.ReadCloser
	close func() error
}

func (s *backendExportStream) Close() error { return s.close() }

// Export ignores name because snapshot.Pusher conflates the source identifier with the destination repository while the CLI allows them to differ.
func (e backendExporter) Export(ctx context.Context, _ string) (io.ReadCloser, func() error, error) {
	r, err := e.backend.Export(ctx, e.ref)
	if err != nil {
		return nil, nil, err
	}
	close := sync.OnceValue(r.Close)
	return &backendExportStream{ReadCloser: r, close: close}, close, nil
}

func (h Handler) Push(cmd *cobra.Command, args []string) error {
	ctx, conf := h.Init(cmd)
	ref, destination := args[0], args[1]
	target, err := cmdcore.ParseOCIPushTarget(destination)
	if err != nil {
		return err
	}
	backend, err := cmdcore.InitSnapshot(ctx, conf)
	if err != nil {
		return err
	}
	snap, err := backend.Inspect(ctx, ref)
	if err != nil {
		return fmt.Errorf("inspect snapshot %s: %w", ref, err)
	}
	zstdLevel, _ := cmd.Flags().GetInt("zstd-level")
	chunkSizeMiB, _ := cmd.Flags().GetInt("chunk-size-mib")
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	memoryBudgetMiB, _ := cmd.Flags().GetInt("memory-budget-mib")

	logger := log.WithFunc("cmd.snapshot.Push")
	logger.Infof(ctx, "pushing snapshot %s to %s ...", ref, destination)
	result, err := (&commonsnapshot.Pusher{
		Uploader: target.Registry,
		Cocoon:   backendExporter{backend: backend, ref: snap.ID},
	}).Push(ctx, commonsnapshot.PushOptions{
		Name:            target.Repository,
		Tag:             target.Tag,
		BaseImage:       snap.Image,
		ZstdLevel:       zstdLevel,
		ChunkSizeMiB:    chunkSizeMiB,
		Concurrency:     concurrency,
		MemoryBudgetMiB: memoryBudgetMiB,
		Progress: func(line string) {
			logger.Info(ctx, line)
		},
	})
	if err != nil {
		return fmt.Errorf("push snapshot: %w", err)
	}
	fmt.Println(result.ManifestDigest)
	return nil
}
