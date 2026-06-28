package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/utils"
)

// layerJob carries the per-layer processing context (one struct per worker).
type layerJob struct {
	conf           *Config
	idx, total     int
	layer          v1.Layer
	workDir        string
	knownBootHexes map[string]struct{}
	tracker        progress.Tracker
	result         *pullLayerResult
}

func processLayer(ctx context.Context, j layerJob) error {
	logger := log.WithFunc("oci.processLayer")

	layerDigest, err := j.layer.Digest()
	if err != nil {
		return fmt.Errorf("get digest: %w", err)
	}
	digestHex := layerDigest.Hex

	j.result.index = j.idx
	j.result.digest = images.NewDigest(digestHex)

	if utils.ValidFile(j.conf.BlobPath(digestHex)) {
		handleCachedLayer(ctx, j, digestHex)
		return nil
	}

	logger.Debugf(ctx, "Layer %d: sha256:%s -> erofs (single-pass)", j.idx, digestHex[:12])

	layerDir := filepath.Join(j.workDir, fmt.Sprintf("layer-%d", j.idx))
	if err = os.MkdirAll(layerDir, 0o750); err != nil {
		return fmt.Errorf("create layer work dir: %w", err)
	}

	rc, err := j.layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("open uncompressed layer: %w", err)
	}
	defer rc.Close() //nolint:errcheck

	erofsPath := filepath.Join(layerDir, digestHex+".erofs")
	layerUUID := utils.UUIDv5(digestHex)

	kernelPath, initrdPath, err := runErofsConversion(ctx, rc, layerDir, digestHex, layerUUID, erofsPath)
	if err != nil {
		return err
	}

	j.result.kernelPath = kernelPath
	j.result.initrdPath = initrdPath
	j.result.erofsPath = erofsPath
	j.tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: j.idx, Total: j.total, Digest: digestHex[:12]})
	return nil
}

func handleCachedLayer(ctx context.Context, j layerJob, digestHex string) {
	logger := log.WithFunc("oci.handleCachedLayer")
	logger.Debugf(ctx, "Layer %d: sha256:%s already cached", j.idx, digestHex[:12])
	applyCachedLayerPaths(j.conf, j.result, digestHex)
	selfHealBootFiles(ctx, j, digestHex)
	j.tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: j.idx, Total: j.total, Digest: digestHex[:12]})
}

// applyCachedLayerPaths fills result with cached blob/kernel/initrd paths for digestHex; caller asserts the erofs blob exists, kernel/initrd are best-effort.
func applyCachedLayerPaths(conf *Config, result *pullLayerResult, digestHex string) {
	result.erofsPath = conf.BlobPath(digestHex)
	if utils.ValidFile(conf.KernelPath(digestHex)) {
		result.kernelPath = conf.KernelPath(digestHex)
	}
	if utils.ValidFile(conf.InitrdPath(digestHex)) {
		result.initrdPath = conf.InitrdPath(digestHex)
	}
}
