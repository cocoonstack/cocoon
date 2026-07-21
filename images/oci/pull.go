package oci

import (
	"context"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/utils"
)

type pullLayerResult struct {
	index      int
	digest     images.Digest
	erofsPath  string
	kernelPath string
	initrdPath string
}

func pull(ctx context.Context, conf *Config, store *images.Store[imageEntry], imageRef string, tracker progress.Tracker) error {
	logger := log.WithFunc("oci.pull")

	ref, digestHex, layers, err := fetchImage(ctx, imageRef)
	if err != nil {
		return err
	}

	// Heavy work (downloads, EROFS conversion, blob renames) runs outside any transaction under finishImport's per-digest locks; the up-to-date pre-check is racy but benign since the whole flow is idempotent per digest.
	var upToDate bool
	var knownBootHexes map[string]struct{}
	if err := store.View(ctx, func(idx *imageIndex) error {
		upToDate = isUpToDate(conf, idx, ref, digestHex)
		knownBootHexes = collectBootHexes(idx)
		return nil
	}); err != nil {
		return err
	}
	if upToDate {
		logger.Debugf(ctx, "Already up to date: %s (digest: sha256:%s)", ref, digestHex)
		return nil
	}

	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhasePull, Index: -1, Total: len(layers)})

	workDir, cleanup, mkErr := newWorkDir(conf, "pull-*")
	if mkErr != nil {
		return mkErr
	}
	defer cleanup()

	results, waitErr := processLayers(ctx, conf, layers, workDir, knownBootHexes, tracker)
	if waitErr != nil {
		return waitErr
	}

	healCachedBootFiles(ctx, conf, layers, results, workDir)

	return finishImport(ctx, conf, store, ref, images.NewDigest(digestHex), results, tracker, "Pulled")
}

func processLayers(ctx context.Context, conf *Config, layers []v1.Layer, workDir string, knownBootHexes map[string]struct{}, tracker progress.Tracker) ([]pullLayerResult, error) {
	totalLayers := len(layers)
	results, waitErr := utils.Map(ctx, layers, func(ctx context.Context, i int, layer v1.Layer) (pullLayerResult, error) {
		var r pullLayerResult
		err := processLayer(ctx, layerJob{
			conf: conf, idx: i, total: totalLayers, layer: layer,
			workDir: workDir, knownBootHexes: knownBootHexes,
			tracker: tracker, result: &r,
		})
		return r, err
	}, conf.PoolSize)
	if waitErr != nil {
		return nil, fmt.Errorf("process layers: %w", waitErr)
	}
	return results, nil
}
