package oci

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

// IsTarFile checks if a file is a tar archive by reading its first header.
func IsTarFile(path string) bool {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck

	tr := tar.NewReader(f)
	_, err = tr.Next()
	return err == nil
}

// importTarLayers imports local tar files as OCI layers.
func importTarLayers(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, file ...string) error {
	logger := log.WithFunc("oci.import")

	if len(file) == 0 {
		return fmt.Errorf("no tar files provided")
	}
	for _, f := range file {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("file %s: %w", f, err)
		}
	}

	return store.Update(ctx, func(idx *imageIndex) error {
		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhasePull, Index: -1, Total: len(file)})

		workDir, err := os.MkdirTemp(conf.TempDir(), "import-*")
		if err != nil {
			return fmt.Errorf("create work dir: %w", err)
		}
		defer os.RemoveAll(workDir) //nolint:errcheck

		totalLayers := len(file)
		results, mapErr := utils.Map(ctx, file, func(ctx context.Context, i int, filePath string) (pullLayerResult, error) {
			var r pullLayerResult
			err := processLocalTar(ctx, conf, i, totalLayers, filePath, workDir, tracker, &r)
			return r, err
		}, conf.Root.EffectivePoolSize())
		if mapErr != nil {
			return fmt.Errorf("process layers: %w", mapErr)
		}

		manifestDigest := computeManifestDigest(results)

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseCommit, Index: -1, Total: len(results)})
		if err := commitAndRecord(conf, idx, name, manifestDigest, results); err != nil {
			return err
		}

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseDone, Index: -1, Total: len(results)})
		logger.Infof(ctx, "Imported: %s (digest: %s, layers: %d)", name, manifestDigest, len(results))
		return nil
	})
}

// importTarFromReader imports a single tar layer from a reader.
func importTarFromReader(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, r io.Reader) error {
	logger := log.WithFunc("oci.import")

	return store.Update(ctx, func(idx *imageIndex) error {
		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhasePull, Index: -1, Total: 1})

		workDir, err := os.MkdirTemp(conf.TempDir(), "import-*")
		if err != nil {
			return fmt.Errorf("create work dir: %w", err)
		}
		defer os.RemoveAll(workDir) //nolint:errcheck

		var result pullLayerResult
		if err := processTarReader(ctx, conf, 0, 1, r, name, workDir, tracker, &result); err != nil {
			return fmt.Errorf("process layer: %w", err)
		}

		manifestDigest := computeManifestDigest([]pullLayerResult{result})

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseCommit, Index: -1, Total: 1})
		if err := commitAndRecord(conf, idx, name, manifestDigest, []pullLayerResult{result}); err != nil {
			return err
		}

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseDone, Index: -1, Total: 1})
		logger.Infof(ctx, "Imported: %s (digest: %s, layers: 1)", name, manifestDigest)
		return nil
	})
}

// processLocalTar opens a local tar and forwards it to processTarReader.
func processLocalTar(ctx context.Context, conf *Config, idx, total int, tarPath, workDir string, tracker progress.Tracker, result *pullLayerResult) error {
	f, err := os.Open(tarPath) //nolint:gosec // user-provided import file
	if err != nil {
		return fmt.Errorf("open %s: %w", tarPath, err)
	}
	defer f.Close() //nolint:errcheck

	return processTarReader(ctx, conf, idx, total, f, tarPath, workDir, tracker, result)
}

// processTarReader hashes, converts, and scans one tar stream in a single pass.
func processTarReader(ctx context.Context, conf *Config, idx, total int, r io.Reader, label, workDir string, tracker progress.Tracker, result *pullLayerResult) error {
	logger := log.WithFunc("oci.processTarReader")

	result.index = idx

	layerDir := filepath.Join(workDir, fmt.Sprintf("layer-%d", idx))
	if err := os.MkdirAll(layerDir, 0o750); err != nil {
		return fmt.Errorf("create layer work dir: %w", err)
	}

	hasher := sha256.New()
	teeForHash := io.TeeReader(r, hasher)

	// Write EROFS to a temp path until the digest is known.
	tmpErofsPath := filepath.Join(layerDir, fmt.Sprintf("layer-%d.erofs", idx))
	tmpUUID := utils.UUIDv5(fmt.Sprintf("import-%s-%d", label, idx))

	cmd, erofsStdin, output, err := startErofsConversion(ctx, tmpUUID, tmpErofsPath)
	if err != nil {
		return fmt.Errorf("start erofs conversion: %w", err)
	}

	teeForErofs := io.TeeReader(teeForHash, erofsStdin)

	kernelPath, initrdPath, scanErr := scanBootFiles(ctx, teeForErofs, layerDir, fmt.Sprintf("import-%d", idx))

	// Drain the rest so the hasher and mkfs.erofs see the full stream.
	if scanErr == nil {
		if _, drainErr := io.Copy(io.Discard, teeForErofs); drainErr != nil {
			scanErr = fmt.Errorf("drain tar stream: %w", drainErr)
		}
	}
	_ = erofsStdin.Close()

	if waitErr := cmd.Wait(); waitErr != nil {
		return fmt.Errorf("mkfs.erofs failed: %w (output: %s)", waitErr, output.String())
	}
	if scanErr != nil {
		return fmt.Errorf("scan boot files: %w", scanErr)
	}

	digestHex := hex.EncodeToString(hasher.Sum(nil))
	result.digest = images.NewDigest(digestHex)

	if utils.ValidFile(conf.BlobPath(digestHex)) {
		logger.Debugf(ctx, "Layer %d: sha256:%s already cached", idx, digestHex[:12])
		result.erofsPath = conf.BlobPath(digestHex)
		if utils.ValidFile(conf.KernelPath(digestHex)) {
			result.kernelPath = conf.KernelPath(digestHex)
		}
		if utils.ValidFile(conf.InitrdPath(digestHex)) {
			result.initrdPath = conf.InitrdPath(digestHex)
		}
	} else {
		result.erofsPath = tmpErofsPath
	}

	if err := renameBootFiles(layerDir, digestHex, kernelPath, initrdPath, result); err != nil {
		return err
	}

	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: idx, Total: total, Digest: digestHex[:12]})
	return nil
}

// renameBootFiles moves extracted kernel/initrd temps to digest-based names.
func renameBootFiles(baseDir, digestHex, kernelPath, initrdPath string, result *pullLayerResult) error {
	type bootFile struct {
		src  string
		dst  *string
		name string
	}
	for _, bf := range []bootFile{
		{kernelPath, &result.kernelPath, digestHex + ".vmlinuz"},
		{initrdPath, &result.initrdPath, digestHex + ".initrd.img"},
	} {
		if bf.src == "" || *bf.dst != "" {
			continue
		}
		clean := filepath.Clean(bf.src)
		if !filepath.IsAbs(clean) || filepath.Dir(clean) != filepath.Clean(baseDir) {
			return fmt.Errorf("path %q escapes base dir", bf.src)
		}
		dst := filepath.Join(baseDir, bf.name)
		if err := os.Rename(clean, dst); err != nil { //nolint:gosec // path validated above
			return fmt.Errorf("rename %s: %w", bf.name, err)
		}
		*bf.dst = dst
	}
	return nil
}

// computeManifestDigest computes a synthetic manifest digest from all layer digests.
func computeManifestDigest(results []pullLayerResult) images.Digest {
	h := sha256.New()
	for _, r := range results {
		h.Write([]byte(r.digest.Hex()))
	}
	return images.NewDigest(hex.EncodeToString(h.Sum(nil)))
}
