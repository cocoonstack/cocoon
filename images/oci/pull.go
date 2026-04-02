package oci

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	ociProgress "github.com/cocoonstack/cocoon/progress/oci"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/utils"
)

// pullLayerResult holds the output of processing a single layer.
type pullLayerResult struct {
	index      int
	digest     images.Digest
	erofsPath  string
	kernelPath string // non-empty if this layer contains a kernel
	initrdPath string // non-empty if this layer contains an initrd
}

// pull downloads an OCI image, extracts boot files, and converts each layer
// to EROFS concurrently.
func pull(ctx context.Context, conf *Config, store storage.Store[imageIndex], imageRef string, tracker progress.Tracker) error {
	logger := log.WithFunc("oci.pull")

	// Phase 1: network I/O — no lock held.
	ref, digestHex, layers, err := fetchImage(ctx, imageRef)
	if err != nil {
		return err
	}

	// Phase 2: lock → idempotency check → process layers → commit.
	// GC uses the same locker, so it will wait until we finish.
	return store.Update(ctx, func(idx *imageIndex) error {
		// Idempotency: check if already pulled with same manifest and all files intact.
		if isUpToDate(conf, idx, ref, digestHex) {
			logger.Debugf(ctx, "Already up to date: %s (digest: sha256:%s)", ref, digestHex)
			return nil
		}

		// Collect known boot layer digests from ALL entries for cross-image self-heal.
		knownBootHexes := collectBootHexes(idx)

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhasePull, Index: -1, Total: len(layers)})

		workDir, mkErr := os.MkdirTemp(conf.TempDir(), "pull-*")
		if mkErr != nil {
			return fmt.Errorf("create work dir: %w", mkErr)
		}
		defer os.RemoveAll(workDir) //nolint:errcheck

		// Process layers concurrently with bounded parallelism.
		totalLayers := len(layers)
		results, waitErr := utils.Map(ctx, layers, func(ctx context.Context, i int, layer v1.Layer) (pullLayerResult, error) {
			var r pullLayerResult
			err := processLayer(ctx, conf, i, totalLayers, layer, workDir, knownBootHexes, tracker, &r)
			return r, err
		}, conf.Root.EffectivePoolSize())
		if waitErr != nil {
			return fmt.Errorf("process layers: %w", waitErr)
		}

		healCachedBootFiles(ctx, conf, layers, results, workDir)

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseCommit, Index: -1, Total: len(results)})
		manifestDigest := images.NewDigest(digestHex)
		if err := commitAndRecord(conf, idx, ref, manifestDigest, results); err != nil {
			return err
		}

		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseDone, Index: -1, Total: len(results)})
		logger.Infof(ctx, "Pulled: %s (digest: sha256:%s, layers: %d)", ref, digestHex, len(results))
		return nil
	})
}

// fetchImage resolves the image reference, fetches the manifest, and returns
// the layer descriptors. No lock is held — this is pure network I/O.
func fetchImage(ctx context.Context, imageRef string) (ref, digestHex string, layers []v1.Layer, err error) {
	logger := log.WithFunc("oci.pull")

	parsedRef, parseErr := name.ParseReference(imageRef)
	if parseErr != nil {
		return "", "", nil, fmt.Errorf("invalid image reference %q: %w", imageRef, parseErr)
	}
	ref = parsedRef.String()

	platform := v1.Platform{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	}

	logger.Debugf(ctx, "Pulling image: %s", ref)

	img, fetchErr := remote.Image(parsedRef,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	)
	if fetchErr != nil {
		return "", "", nil, fmt.Errorf("fetch image %s: %w", ref, fetchErr)
	}

	manifest, digestErr := img.Digest()
	if digestErr != nil {
		return "", "", nil, fmt.Errorf("get manifest digest: %w", digestErr)
	}
	digestHex = manifest.Hex

	layers, layersErr := img.Layers()
	if layersErr != nil {
		return "", "", nil, fmt.Errorf("get layers: %w", layersErr)
	}
	if len(layers) == 0 {
		return "", "", nil, fmt.Errorf("image %s has no layers", ref)
	}

	return ref, digestHex, layers, nil
}

// isUpToDate checks if the image is already pulled with the same manifest digest
// and all files (blobs, kernel, initrd) are intact on disk.
func isUpToDate(conf *Config, idx *imageIndex, ref, digestHex string) bool {
	entry, ok := idx.Images[ref]
	if !ok || entry == nil || entry.ManifestDigest != images.NewDigest(digestHex) {
		return false
	}
	if !utils.ValidFile(conf.KernelPath(entry.KernelLayer.Hex())) ||
		!utils.ValidFile(conf.InitrdPath(entry.InitrdLayer.Hex())) {
		return false
	}
	for _, layer := range entry.Layers {
		if !utils.ValidFile(conf.BlobPath(layer.Digest.Hex())) {
			return false
		}
	}
	return true
}

// collectBootHexes gathers boot layer digests from ALL index entries
// for cross-image self-heal during processLayer.
func collectBootHexes(idx *imageIndex) map[string]struct{} {
	hexes := make(map[string]struct{})
	for _, e := range idx.Images {
		if e == nil {
			continue
		}
		if e.KernelLayer != "" {
			hexes[e.KernelLayer.Hex()] = struct{}{}
		}
		if e.InitrdLayer != "" {
			hexes[e.InitrdLayer.Hex()] = struct{}{}
		}
	}
	return hexes
}

// moveBootFile renames a boot artifact (kernel or initrd) to its shared path,
// creating the boot directory if needed. No-op if src is empty or already in place.
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
			return
		}
	}
	return
}

// commitAndRecord moves artifacts to shared image paths and records the image entry.
// Must be called under flock (inside idx.Update).
func commitAndRecord(conf *Config, idx *imageIndex, ref string, manifestDigest images.Digest, results []pullLayerResult) error {
	var (
		layerEntries []layerEntry
		kernelLayer  images.Digest
		initrdLayer  images.Digest
	)

	for i := range results {
		r := &results[i]
		layerDigestHex := r.digest.Hex()

		// Move erofs to shared blob path if not already there.
		if r.erofsPath != conf.BlobPath(layerDigestHex) {
			if err := os.Rename(r.erofsPath, conf.BlobPath(layerDigestHex)); err != nil {
				return fmt.Errorf("move layer %d erofs: %w", r.index, err)
			}
		}

		// Move boot files to shared boot dir if not already there.
		if err := moveBootFile(r.kernelPath, conf.KernelPath(layerDigestHex), conf.BootDir(layerDigestHex), r.index, "kernel"); err != nil {
			return err
		}
		if err := moveBootFile(r.initrdPath, conf.InitrdPath(layerDigestHex), conf.BootDir(layerDigestHex), r.index, "initrd"); err != nil {
			return err
		}

		// Track which layer provides boot files (later layers win per OCI ordering).
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

	// Final validation: ensure all artifacts still exist on disk.
	// Guards against concurrent GC deleting cached blobs/boot files between
	// processLayer (no flock) and this point (under flock).
	for _, le := range layerEntries {
		if !utils.ValidFile(conf.BlobPath(le.Digest.Hex())) {
			return fmt.Errorf("blob missing for layer %s (concurrent GC?)", le.Digest)
		}
	}
	if !utils.ValidFile(conf.KernelPath(kernelLayer.Hex())) {
		return fmt.Errorf("kernel missing for %s (concurrent GC?)", kernelLayer)
	}
	if !utils.ValidFile(conf.InitrdPath(initrdLayer.Hex())) {
		return fmt.Errorf("initrd missing for %s (concurrent GC?)", initrdLayer)
	}

	// Compute total on-disk size of all artifacts.
	var totalSize int64
	for _, le := range layerEntries {
		if info, err := os.Stat(conf.BlobPath(le.Digest.Hex())); err == nil {
			totalSize += info.Size()
		}
	}
	if info, err := os.Stat(conf.KernelPath(kernelLayer.Hex())); err == nil {
		totalSize += info.Size()
	}
	if info, err := os.Stat(conf.InitrdPath(initrdLayer.Hex())); err == nil {
		totalSize += info.Size()
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

// healCachedBootFiles force-scans cached layers to recover missing boot files.
// processLayer's first-pass self-heal is evidence-based: it only re-extracts
// boot files when it has positive evidence (existing boot file, boot dir on
// disk, or knownBootHexes from the index). This leaves a blind spot when all
// evidence has been erased (every entry referencing the boot layer deleted and
// GC removed the boot dir) but the erofs blob is still cached.
// healCachedBootFiles closes that gap by running after all layers are processed:
// if kernel or initrd (or both) are still missing across results, it sequentially
// re-scans every cached layer to find them.
func healCachedBootFiles(ctx context.Context, conf *Config, layers []v1.Layer, results []pullLayerResult, workDir string) {
	logger := log.WithFunc("oci.healCachedBootFiles")

	hasKernel, hasInitrd := bootFilesPresent(results)
	if hasKernel && hasInitrd {
		return
	}

	logger.Warnf(ctx, "Boot files incomplete after first pass (kernel=%v, initrd=%v), force-scanning cached layers", hasKernel, hasInitrd)

	for i, layer := range layers {
		digestHex := results[i].digest.Hex()
		// Only re-scan cached layers; freshly processed layers were already fully scanned.
		if results[i].erofsPath != conf.BlobPath(digestHex) {
			continue
		}
		kp, ip := recoverBootFiles(ctx, layer, workDir, i, digestHex)
		if results[i].kernelPath == "" && kp != "" {
			results[i].kernelPath = kp
		}
		if results[i].initrdPath == "" && ip != "" {
			results[i].initrdPath = ip
		}
	}
}

// processLayer handles a single layer: extracts boot files and converts to EROFS
// in a single pass using io.TeeReader. If the layer is already cached, it checks
// for missing boot files and self-heals by re-extracting them.
// knownBootHexes contains digest hex strings of layers previously recorded as boot
// layers in the index, enabling targeted self-heal even when bootDir is deleted.
func processLayer(ctx context.Context, conf *Config, idx, total int, layer v1.Layer, workDir string, knownBootHexes map[string]struct{}, tracker progress.Tracker, result *pullLayerResult) error {
	logger := log.WithFunc("oci.processLayer")

	layerDigest, err := layer.Digest()
	if err != nil {
		return fmt.Errorf("get digest: %w", err)
	}
	digestHex := layerDigest.Hex

	result.index = idx
	result.digest = images.NewDigest(digestHex)

	// Check if this layer's blob already exists and is valid (shared across images).
	if utils.ValidFile(conf.BlobPath(digestHex)) {
		handleCachedLayer(ctx, conf, layer, workDir, idx, total, digestHex, knownBootHexes, tracker, result)
		return nil
	}

	logger.Debugf(ctx, "Layer %d: sha256:%s -> erofs (single-pass)", idx, digestHex[:12])

	// Per-layer work subdirectory avoids temp file conflicts when
	// a manifest references the same digest more than once.
	layerDir := filepath.Join(workDir, fmt.Sprintf("layer-%d", idx))
	if err = os.MkdirAll(layerDir, 0o750); err != nil {
		return fmt.Errorf("create layer work dir: %w", err)
	}

	// Open uncompressed tar stream once.
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("open uncompressed layer: %w", err)
	}
	defer rc.Close() //nolint:errcheck

	erofsPath := filepath.Join(layerDir, digestHex+".erofs")
	layerUUID := utils.UUIDv5(digestHex)

	// Start mkfs.erofs in background, receiving the tar stream via pipe.
	cmd, erofsStdin, output, err := startErofsConversion(ctx, layerUUID, erofsPath)
	if err != nil {
		return fmt.Errorf("start erofs conversion: %w", err)
	}

	// TeeReader: every byte read for boot scanning also feeds mkfs.erofs.
	tee := io.TeeReader(rc, erofsStdin)
	kernelPath, initrdPath, scanErr := scanBootFiles(ctx, tee, layerDir, digestHex)

	// Drain remaining tar data to ensure mkfs.erofs receives the complete stream.
	if scanErr == nil {
		if _, drainErr := io.Copy(io.Discard, tee); drainErr != nil {
			scanErr = fmt.Errorf("drain layer stream: %w", drainErr)
		}
	}
	_ = erofsStdin.Close()

	if waitErr := cmd.Wait(); waitErr != nil {
		return fmt.Errorf("mkfs.erofs failed: %w (output: %s)", waitErr, output.String())
	}
	if scanErr != nil {
		return fmt.Errorf("scan boot files: %w", scanErr)
	}

	result.kernelPath = kernelPath
	result.initrdPath = initrdPath
	result.erofsPath = erofsPath
	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: idx, Total: total, Digest: digestHex[:12]})
	return nil
}

// handleCachedLayer handles already-cached layers: checks boot files and self-heals if needed.
func handleCachedLayer(ctx context.Context, conf *Config, layer v1.Layer, workDir string, idx, total int, digestHex string, knownBootHexes map[string]struct{}, tracker progress.Tracker, result *pullLayerResult) {
	logger := log.WithFunc("oci.processLayer")
	logger.Debugf(ctx, "Layer %d: sha256:%s already cached", idx, digestHex[:12])
	result.erofsPath = conf.BlobPath(digestHex)

	if utils.ValidFile(conf.KernelPath(digestHex)) {
		result.kernelPath = conf.KernelPath(digestHex)
	}
	if utils.ValidFile(conf.InitrdPath(digestHex)) {
		result.initrdPath = conf.InitrdPath(digestHex)
	}

	selfHealBootFiles(ctx, conf, layer, workDir, idx, digestHex, knownBootHexes, result)

	tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: idx, Total: total, Digest: digestHex[:12]})
}

// selfHealBootFiles re-extracts missing boot files from a cached layer when
// evidence suggests it is a boot layer. Evidence sources: existing boot file,
// boot dir on disk, or index records this digest as a boot layer.
func selfHealBootFiles(ctx context.Context, conf *Config, layer v1.Layer, workDir string, idx int, digestHex string, knownBootHexes map[string]struct{}, result *pullLayerResult) {
	if result.kernelPath != "" && result.initrdPath != "" {
		return
	}

	hasEvidence := result.kernelPath != "" || result.initrdPath != ""
	if !hasEvidence {
		_, statErr := os.Stat(conf.BootDir(digestHex))
		hasEvidence = statErr == nil
	}
	if !hasEvidence {
		_, hasEvidence = knownBootHexes[digestHex]
	}
	if !hasEvidence {
		return
	}

	log.WithFunc("oci.processLayer").Warnf(ctx, "Layer %d: sha256:%s attempting boot file recovery", idx, digestHex[:12])
	kp, ip := recoverBootFiles(ctx, layer, workDir, idx, digestHex)
	if result.kernelPath == "" {
		result.kernelPath = kp
	}
	if result.initrdPath == "" {
		result.initrdPath = ip
	}
}

// recoverBootFiles re-extracts boot files from a layer into a heal subdirectory.
// Returns extracted kernel and initrd paths (empty if not found or on error).
func recoverBootFiles(ctx context.Context, layer v1.Layer, workDir string, idx int, digestHex string) (kernelPath, initrdPath string) {
	logger := log.WithFunc("oci.recoverBootFiles")
	healDir := filepath.Join(workDir, fmt.Sprintf("heal-%d", idx))
	if err := os.MkdirAll(healDir, 0o750); err != nil {
		logger.Warnf(ctx, "Layer %d: cannot create heal dir: %v", idx, err)
		return "", ""
	}
	rc, err := layer.Uncompressed()
	if err != nil {
		logger.Warnf(ctx, "Layer %d: cannot open for boot scan: %v", idx, err)
		return "", ""
	}
	kp, ip, scanErr := scanBootFiles(ctx, rc, healDir, digestHex)
	_ = rc.Close()
	if scanErr != nil {
		logger.Warnf(ctx, "Layer %d: boot scan failed: %v", idx, scanErr)
		return "", ""
	}
	return kp, ip
}

// scanBootFiles reads a tar stream and extracts kernel/initrd files.
// Accepts both tar.TypeReg and deprecated tar.TypeRegA. Excludes .old variants.
// Files are written to workDir with digest-based names.
func scanBootFiles(ctx context.Context, r io.Reader, workDir, digestHex string) (kernelPath, initrdPath string, err error) {
	logger := log.WithFunc("oci.scanBootFiles")

	tr := tar.NewReader(r)
	for {
		hdr, readErr := tr.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read tar entry: %w", readErr)
		}

		// Accept regular files only (TypeReg and deprecated TypeRegA '\x00').
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // accept deprecated TypeRegA from legacy tar writers
			continue
		}

		entryName := filepath.Clean(hdr.Name)
		base := filepath.Base(entryName)

		// Exclude .old variants (vmlinuz.old, initrd.img.old).
		if strings.HasSuffix(base, ".old") {
			continue
		}

		isKernel := strings.HasPrefix(base, "vmlinuz")
		isInitrd := strings.HasPrefix(base, "initrd.img")
		if !isKernel && !isInitrd {
			continue
		}

		// Only extract files under boot/ or at top level.
		dir := filepath.Dir(entryName)
		if dir != "boot" && dir != "." {
			continue
		}

		var dstPath string
		if isKernel {
			dstPath = filepath.Join(workDir, digestHex+".vmlinuz")
		} else {
			dstPath = filepath.Join(workDir, digestHex+".initrd.img")
		}

		f, createErr := os.Create(dstPath) //nolint:gosec // internal temp file
		if createErr != nil {
			return "", "", fmt.Errorf("create %s: %w", filepath.Base(dstPath), createErr)
		}
		if _, copyErr := io.Copy(f, tr); copyErr != nil { //nolint:gosec // boot files from trusted OCI layer tar
			_ = f.Close()
			return "", "", fmt.Errorf("write %s: %w", filepath.Base(dstPath), copyErr)
		}
		_ = f.Close()

		if isKernel {
			kernelPath = dstPath
		} else {
			initrdPath = dstPath
		}
		logger.Debugf(ctx, "Layer %s: extracted %s", digestHex[:12], base)
	}
	return kernelPath, initrdPath, nil
}
