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

// finishImport commits results under name and emits the shared commit/done
// progress + log tail; verb names the operation ("Pulled"/"Imported").
func finishImport(ctx context.Context, conf *Config, idx *imageIndex, name string, manifestDigest images.Digest, results []pullLayerResult, tracker progress.Tracker, verb string) error {
	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseCommit, Index: -1, Total: len(results)})
	if err := commitAndRecord(conf, idx, name, manifestDigest, results); err != nil {
		return err
	}
	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseDone, Index: -1, Total: len(results)})
	log.WithFunc("oci.finishImport").Infof(ctx, "%s: %s (digest: %s, layers: %d)", verb, name, manifestDigest, len(results))
	return nil
}

func commitAndRecord(conf *Config, idx *imageIndex, ref string, manifestDigest images.Digest, results []pullLayerResult) error {
	var (
		layerEntries []layerEntry
		kernelLayer  images.Digest
		initrdLayer  images.Digest
	)

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

		if r.kernelPath != "" {
			kernelLayer = r.digest
		}
		if r.initrdPath != "" {
			initrdLayer = r.digest
		}

		layerEntries = append(layerEntries, layerEntry{Digest: r.digest})
	}

	if kernelLayer == "" || initrdLayer == "" {
		return fmt.Errorf("image %s missing boot files (vmlinuz/initrd.img)", ref)
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
			return err
		}
	}
	if err := addSize(conf.KernelPath(kernelLayer.Hex()), fmt.Sprintf("kernel missing for %s", kernelLayer)); err != nil {
		return err
	}
	if err := addSize(conf.InitrdPath(initrdLayer.Hex()), fmt.Sprintf("initrd missing for %s", initrdLayer)); err != nil {
		return err
	}

	idx.Images[ref] = &imageEntry{
		Ref:            ref,
		ManifestDigest: manifestDigest,
		Layers:         layerEntries,
		KernelLayer:    kernelLayer,
		InitrdLayer:    initrdLayer,
		Size:           totalSize,
		CreatedAt:      time.Now(),
	}
	return nil
}
