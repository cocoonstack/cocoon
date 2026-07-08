package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	pullLayerAttempts = 3
	pullLayerBackoff  = 2 * time.Second
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

	// Hoisted out of the retried region: a too-old mkfs.erofs is permanent,
	// retrying it would burn attempts, backoff sleeps, and aborted blob GETs.
	if err = checkErofsVersion(ctx); err != nil {
		return err
	}

	layerDir := filepath.Join(j.workDir, fmt.Sprintf("layer-%d", j.idx))
	erofsPath := filepath.Join(layerDir, digestHex+".erofs")
	layerUUID := utils.UUIDv5(digestHex)

	var kernelPath, initrdPath string
	err = retryLayer(ctx, pullLayerAttempts, pullLayerBackoff, func() error {
		var convErr error
		kernelPath, initrdPath, convErr = convertLayer(ctx, j, layerDir, digestHex, layerUUID, erofsPath)
		if convErr != nil {
			logger.Warnf(ctx, "layer %d (sha256:%s): %v", j.idx, digestHex[:12], convErr)
		}
		return convErr
	})
	if err != nil {
		return err
	}

	j.result.kernelPath = kernelPath
	j.result.initrdPath = initrdPath
	j.result.erofsPath = erofsPath
	j.tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: j.idx, Total: j.total, Digest: digestHex[:12]})
	return nil
}

// convertLayer downloads and converts one layer into a fresh layerDir; the
// remote stream cannot resume, so each attempt redoes the whole layer and
// must not see a previous attempt's partial erofs or scanned boot files.
func convertLayer(ctx context.Context, j layerJob, layerDir, digestHex, layerUUID, erofsPath string) (kernelPath, initrdPath string, err error) {
	if err = os.RemoveAll(layerDir); err != nil {
		return "", "", fmt.Errorf("reset layer work dir: %w", err)
	}
	if err = os.MkdirAll(layerDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create layer work dir: %w", err)
	}
	rc, err := j.layer.Uncompressed()
	if err != nil {
		return "", "", fmt.Errorf("open uncompressed layer: %w", err)
	}
	defer rc.Close() //nolint:errcheck
	return runErofsConversion(ctx, rc, layerDir, digestHex, layerUUID, erofsPath)
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

// retryLayer runs fn up to attempts times, sleeping backoff between failures;
// every error retries — a mid-stream break is indistinguishable from bad input
// by the time mkfs or the boot scan reports it.
func retryLayer(ctx context.Context, attempts int, backoff time.Duration, fn func() error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == attempts || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
	}
}
