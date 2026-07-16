package oci

import (
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

// tarImportJob carries the per-tar context for raw-tar layer import.
type tarImportJob struct {
	conf       *Config
	idx, total int
	label      string // tarPath or "import-<name>"
	workDir    string
	tracker    progress.Tracker
	result     *pullLayerResult
}

func importTarLayers(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, file ...string) error {
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

		workDir, cleanup, err := newWorkDir(conf, "import-*")
		if err != nil {
			return err
		}
		defer cleanup()

		totalLayers := len(file)
		results, mapErr := utils.Map(ctx, file, func(ctx context.Context, i int, filePath string) (pullLayerResult, error) {
			var r pullLayerResult
			err := processLocalTar(ctx, tarImportJob{
				conf: conf, idx: i, total: totalLayers,
				label: filePath, workDir: workDir, tracker: tracker, result: &r,
			}, filePath)
			return r, err
		}, conf.PoolSize)
		if mapErr != nil {
			return fmt.Errorf("process layers: %w", mapErr)
		}

		return finishImport(ctx, conf, idx, name, computeManifestDigest(results), results, tracker, "Imported")
	})
}

func importTarFromReader(ctx context.Context, conf *Config, store storage.Store[imageIndex], name string, tracker progress.Tracker, r io.Reader) error {
	return store.Update(ctx, func(idx *imageIndex) error {
		tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhasePull, Index: -1, Total: 1})

		workDir, cleanup, err := newWorkDir(conf, "import-*")
		if err != nil {
			return err
		}
		defer cleanup()

		var result pullLayerResult
		if err := processTarReader(ctx, tarImportJob{
			conf: conf, idx: 0, total: 1,
			label: name, workDir: workDir, tracker: tracker, result: &result,
		}, r); err != nil {
			return fmt.Errorf("process layer: %w", err)
		}

		results := []pullLayerResult{result}
		return finishImport(ctx, conf, idx, name, computeManifestDigest(results), results, tracker, "Imported")
	})
}

func processLocalTar(ctx context.Context, j tarImportJob, tarPath string) error {
	f, err := os.Open(tarPath) //nolint:gosec // user-provided import file
	if err != nil {
		return fmt.Errorf("open %s: %w", tarPath, err)
	}
	defer f.Close() //nolint:errcheck

	return processTarReader(ctx, j, f)
}

func processTarReader(ctx context.Context, j tarImportJob, r io.Reader) error {
	logger := log.WithFunc("oci.processTarReader")

	j.result.index = j.idx

	layerDir := filepath.Join(j.workDir, fmt.Sprintf("layer-%d", j.idx))
	if err := os.MkdirAll(layerDir, 0o750); err != nil {
		return fmt.Errorf("create layer work dir: %w", err)
	}

	hasher := sha256.New()
	teeForHash := io.TeeReader(r, hasher)

	// Write EROFS to a temp path until the digest is known.
	tmpErofsPath := filepath.Join(layerDir, fmt.Sprintf("layer-%d.erofs", j.idx))
	tmpUUID := utils.UUIDv5(fmt.Sprintf("import-%s-%d", j.label, j.idx))

	kernelPath, initrdPath, err := runErofsConversion(ctx, teeForHash, layerDir, fmt.Sprintf("import-%d", j.idx), tmpUUID, tmpErofsPath)
	if err != nil {
		return err
	}

	digestHex := hex.EncodeToString(hasher.Sum(nil))
	j.result.digest = images.NewDigest(digestHex)

	if utils.ValidFile(j.conf.BlobPath(digestHex)) {
		logger.Debugf(ctx, "Layer %d: sha256:%s already cached", j.idx, digestHex[:12])
		applyCachedLayerPaths(j.conf, j.result, digestHex)
	} else {
		j.result.erofsPath = tmpErofsPath
	}

	if err := renameBootFiles(layerDir, digestHex, kernelPath, initrdPath, j.result); err != nil {
		return err
	}

	j.tracker.OnEvent(ociProgress.Event{Phase: ociProgress.PhaseLayer, Index: j.idx, Total: j.total, Digest: digestHex[:12]})
	return nil
}

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

func computeManifestDigest(results []pullLayerResult) images.Digest {
	h := sha256.New()
	for _, r := range results {
		h.Write([]byte(r.digest.Hex()))
	}
	return images.NewDigest(hex.EncodeToString(h.Sum(nil)))
}
