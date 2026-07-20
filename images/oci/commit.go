package oci

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/utils"
)

func moveBootFile(src, dst, bootDir string, layerIdx int, name string) error {
	if src == "" || src == dst {
		return nil
	}
	if err := os.MkdirAll(bootDir, 0o750); err != nil {
		return fmt.Errorf("create boot dir for layer %d: %w", layerIdx, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move layer %d %s: %w", layerIdx, name, err)
	}
	return nil
}

// finishImport commits blobs, then records the entry in one pure transaction; verb names the operation for logging.
func finishImport(ctx context.Context, conf *Config, store *images.Store[imageEntry], name string, manifestDigest images.Digest, results []pullLayerResult, tracker progress.Tracker, verb string) error {
	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseCommit, Index: -1, Total: len(results)})
	var locks images.BlobLocks
	defer locks.Release()
	lockPaths := make([]string, 0, len(results))
	for i := range results {
		lockPaths = append(lockPaths, conf.BlobLockPath(results[i].digest.Hex()))
	}
	if err := locks.Lock(lockPaths...); err != nil {
		return err
	}
	if err := commitBlobs(conf, results); err != nil {
		return err
	}
	entry, err := buildEntry(conf, name, manifestDigest, results)
	if err != nil {
		return err
	}
	if err := store.Update(ctx, func(idx *imageIndex) error {
		idx.Images[name] = entry
		return nil
	}); err != nil {
		return err
	}
	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseDone, Index: -1, Total: len(results)})
	log.WithFunc("oci.finishImport").Infof(ctx, "%s: %s (digest: %s, layers: %d)", verb, name, manifestDigest, len(results))
	return nil
}

// commitBlobs moves each layer's blob and boot files into their final paths, outside any transaction and under per-digest locks so GC cannot delete a not-yet-indexed blob.
func commitBlobs(conf *Config, results []pullLayerResult) error {
	for i := range results {
		r := &results[i]
		layerDigestHex := r.digest.Hex()

		if r.erofsPath != conf.BlobPath(layerDigestHex) {
			if err := os.Rename(r.erofsPath, conf.BlobPath(layerDigestHex)); err != nil {
				return fmt.Errorf("move layer %d erofs: %w", r.index, err)
			}
		}

		if err := moveBootFile(r.kernelPath, conf.KernelPath(layerDigestHex), conf.BootDir(layerDigestHex), r.index, "kernel"); err != nil {
			return err
		}
		if err := moveBootFile(r.initrdPath, conf.InitrdPath(layerDigestHex), conf.BootDir(layerDigestHex), r.index, "initrd"); err != nil {
			return err
		}
	}
	return nil
}

// buildEntry assembles the index entry from committed blobs; read-only on the
// filesystem, so the enclosing transaction closure stays pure and retryable.
func buildEntry(conf *Config, ref string, manifestDigest images.Digest, results []pullLayerResult) (*imageEntry, error) {
	var (
		layerEntries []layerEntry
		kernelLayer  images.Digest
		initrdLayer  images.Digest
	)
	for i := range results {
		r := &results[i]
		if r.kernelPath != "" {
			kernelLayer = r.digest
		}
		if r.initrdPath != "" {
			initrdLayer = r.digest
		}
		layerEntries = append(layerEntries, layerEntry{Digest: r.digest})
	}
	if kernelLayer == "" || initrdLayer == "" {
		return nil, fmt.Errorf("image %s missing boot files (vmlinuz/initrd.img)", ref)
	}

	var totalSize int64
	addSize := func(path, desc string) error {
		size, err := utils.ValidFileSize(path)
		if err != nil {
			return fmt.Errorf("%s (concurrent GC?): %w", desc, err)
		}
		totalSize += size
		return nil
	}
	for _, le := range layerEntries {
		if err := addSize(conf.BlobPath(le.Digest.Hex()), fmt.Sprintf("blob missing for layer %s", le.Digest)); err != nil {
			return nil, err
		}
	}
	if err := addSize(conf.KernelPath(kernelLayer.Hex()), fmt.Sprintf("kernel missing for %s", kernelLayer)); err != nil {
		return nil, err
	}
	if err := addSize(conf.InitrdPath(initrdLayer.Hex()), fmt.Sprintf("initrd missing for %s", initrdLayer)); err != nil {
		return nil, err
	}

	return &imageEntry{
		Ref:            ref,
		ManifestDigest: manifestDigest,
		Layers:         layerEntries,
		KernelLayer:    kernelLayer,
		InitrdLayer:    initrdLayer,
		Size:           totalSize,
		CreatedAt:      time.Now(),
	}, nil
}
