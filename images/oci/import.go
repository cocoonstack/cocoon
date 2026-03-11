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
	"runtime"

	"golang.org/x/sync/errgroup"

	"github.com/projecteru2/core/log"

	"github.com/projecteru2/cocoon/images"
	"github.com/projecteru2/cocoon/progress"
	ociProgress "github.com/projecteru2/cocoon/progress/oci"
	"github.com/projecteru2/cocoon/storage"
	"github.com/projecteru2/cocoon/utils"
)

// importTarLayers imports local tar files as OCI image layers.
// Each tar file becomes one EROFS layer; boot files (vmlinuz/initrd.img)
// are extracted using the same scanBootFiles logic as pull.
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

		// Process layers concurrently.
		results := make([]pullLayerResult, len(file))
		g, gctx := errgroup.WithContext(ctx)
		limit := conf.Root.PoolSize
		if limit <= 0 {
			limit = runtime.NumCPU()
		}
		g.SetLimit(limit)

		totalLayers := len(file)
		for i, filePath := range file {
			g.Go(func() error {
				return processLocalTar(gctx, conf, i, totalLayers, filePath, workDir, tracker, &results[i])
			})
		}
		if err := g.Wait(); err != nil {
			return fmt.Errorf("process layers: %w", err)
		}

		// Compute manifest digest from layer digests.
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

// processLocalTar processes a single local tar file: computes its digest,
// converts it to EROFS, and extracts boot files — all in a single pass.
func processLocalTar(ctx context.Context, conf *Config, idx, total int, tarPath, workDir string, tracker progress.Tracker, result *pullLayerResult) error {
	logger := log.WithFunc("oci.processLocalTar")

	result.index = idx

	// Per-layer work subdirectory.
	layerDir := filepath.Join(workDir, fmt.Sprintf("layer-%d", idx))
	if err := os.MkdirAll(layerDir, 0o750); err != nil {
		return fmt.Errorf("create layer work dir: %w", err)
	}

	// Open the tar file.
	f, err := os.Open(tarPath) //nolint:gosec // user-provided import file
	if err != nil {
		return fmt.Errorf("open %s: %w", tarPath, err)
	}
	defer f.Close() //nolint:errcheck

	// Compute SHA-256 while streaming through to erofs + boot scan.
	hasher := sha256.New()
	teeForHash := io.TeeReader(f, hasher)

	// We need to: 1) hash all bytes, 2) feed tar to erofs, 3) scan for boot files.
	// Use a temp erofs path first, rename after we know the digest.
	tmpErofsPath := filepath.Join(layerDir, fmt.Sprintf("layer-%d.erofs", idx))
	tmpUUID := utils.UUIDv5(fmt.Sprintf("import-%s-%d", tarPath, idx))

	cmd, erofsStdin, output, err := startErofsConversion(ctx, tmpUUID, tmpErofsPath)
	if err != nil {
		return fmt.Errorf("start erofs conversion: %w", err)
	}

	// TeeReader chain: file → hasher → (tee to erofs stdin) → boot scan
	teeForErofs := io.TeeReader(teeForHash, erofsStdin)

	// Scan boot files from the tar stream (also feeds erofs via tee chain).
	kernelPath, initrdPath, scanErr := scanBootFiles(ctx, teeForErofs, layerDir, fmt.Sprintf("import-%d", idx))

	// Drain remaining data to ensure hasher and erofs receive everything.
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

	// If this blob already exists, use the cached version.
	if utils.ValidFile(conf.BlobPath(digestHex)) {
		logger.Debugf(ctx, "Layer %d: sha256:%s already cached", idx, digestHex[:12])
		result.erofsPath = conf.BlobPath(digestHex)
		// Check for existing boot files.
		if utils.ValidFile(conf.KernelPath(digestHex)) {
			result.kernelPath = conf.KernelPath(digestHex)
		}
		if utils.ValidFile(conf.InitrdPath(digestHex)) {
			result.initrdPath = conf.InitrdPath(digestHex)
		}
	} else {
		result.erofsPath = tmpErofsPath
	}

	// Rename boot file temps to use the real digest hex.
	if err := renameBootFiles(layerDir, digestHex, kernelPath, initrdPath, result); err != nil {
		return err
	}

	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: idx, Total: total, Digest: digestHex[:12]})
	return nil
}

// renameBootFiles renames extracted kernel/initrd temps into baseDir with digest-based names,
// updating result in place. Skips files that are empty or already set on result.
// Returns an error if any source path escapes baseDir.
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

// IsTarFile checks if a file is a tar archive by reading its magic bytes.
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
