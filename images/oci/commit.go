package oci

import (
	"fmt"
	"os"
	"time"

	"github.com/cocoonstack/cocoon/images"
)

// validFileSize returns file size, validating it's a regular non-empty file.
func validFileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0, fmt.Errorf("invalid file: %s", path)
	}
	return info.Size(), nil
}

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

func bootFilesPresent(results []pullLayerResult) (hasKernel, hasInitrd bool) {
	for i := range results {
		if results[i].kernelPath != "" {
			hasKernel = true
		}
		if results[i].initrdPath != "" {
			hasInitrd = true
		}
		if hasKernel && hasInitrd {
			return hasKernel, hasInitrd
		}
	}
	return hasKernel, hasInitrd
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
	for _, le := range layerEntries {
		size, err := validFileSize(conf.BlobPath(le.Digest.Hex()))
		if err != nil {
			return fmt.Errorf("blob missing for layer %s (concurrent GC?)", le.Digest)
		}
		totalSize += size
	}
	if size, err := validFileSize(conf.KernelPath(kernelLayer.Hex())); err != nil {
		return fmt.Errorf("kernel missing for %s (concurrent GC?)", kernelLayer)
	} else {
		totalSize += size
	}
	if size, err := validFileSize(conf.InitrdPath(initrdLayer.Hex())); err != nil {
		return fmt.Errorf("initrd missing for %s (concurrent GC?)", initrdLayer)
	} else {
		totalSize += size
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
