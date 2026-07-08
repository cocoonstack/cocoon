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
	addSize := func(path, desc string) error {
		size, err := validFileSize(path)
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
