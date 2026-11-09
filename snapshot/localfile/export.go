package localfile

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/utils"
)

func (lf *LocalFile) Export(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, false)
}

func (lf *LocalFile) ExportCompressed(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, true)
}

func (lf *LocalFile) ExportToDir(ctx context.Context, ref, dir string) (err error) {
	dataDir, cfg, release, err := lf.DataDir(ctx, ref)
	if err != nil {
		return err
	}
	defer release()
	if err = utils.EnsureDirs(dir); err != nil {
		return err
	}
	// Reject non-empty targets so the export can't merge into an unrelated tree.
	dstEntries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(dstEntries) > 0 {
		return fmt.Errorf("target dir %s is not empty", dir)
	}
	names, err := utils.ListRegularFiles(dataDir)
	if err != nil {
		return err
	}
	attachFiles, err := diskAttachFiles(dataDir)
	if err != nil {
		return err
	}
	// A partial tree would make the retry refuse the non-empty target
	defer func() {
		if err != nil {
			for _, name := range names {
				os.Remove(filepath.Join(dir, name)) //nolint:errcheck,gosec
			}
		}
	}()
	if _, err = utils.Map(ctx, names, func(ctx context.Context, _ int, name string) (struct{}, error) {
		if copyErr := utils.ReflinkCopy(ctx, filepath.Join(dir, name), filepath.Join(dataDir, name), utils.Sync); copyErr != nil {
			return struct{}{}, fmt.Errorf("copy %s: %w", name, copyErr)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	// snapshot.json lands last: it marks the copy complete.
	if err = snapshot.WriteSnapshotEnvelope(dir, cfg, names, attachFiles); err != nil {
		return fmt.Errorf("write envelope: %w", err)
	}
	return nil
}

func (lf *LocalFile) export(ctx context.Context, ref string, compress bool) (io.ReadCloser, error) {
	dataDir, cfg, release, err := lf.DataDir(ctx, ref)
	if err != nil {
		return nil, err
	}

	files, err := utils.ListRegularFiles(dataDir)
	if err != nil {
		release()
		return nil, err
	}
	attachFiles, err := diskAttachFiles(dataDir)
	if err != nil {
		release()
		return nil, err
	}
	jsonData, err := snapshot.MarshalEnvelope(cfg, files, attachFiles)
	if err != nil {
		release()
		return nil, err
	}

	return utils.PipeStream(release, func(w io.Writer) error {
		var gw *gzip.Writer
		if compress {
			var gzErr error
			if gw, gzErr = gzip.NewWriterLevel(w, gzip.BestSpeed); gzErr != nil {
				return fmt.Errorf("create gzip writer: %w", gzErr)
			}
			w = gw
		}
		tw := tar.NewWriter(w)
		if err := tw.WriteHeader(&tar.Header{
			Name:    snapshot.SnapshotJSONName,
			Size:    int64(len(jsonData)),
			Mode:    0o644,
			ModTime: time.Now(),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(jsonData); err != nil {
			return err
		}
		if err := utils.TarDir(tw, dataDir); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		if gw != nil {
			return gw.Close()
		}
		return nil
	}), nil
}

func diskAttachFiles(dataDir string) ([]string, error) {
	meta, err := snapshot.LoadSnapshotMeta(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot metadata: %w", err)
	}
	cowFile, err := snapshot.SnapshotCOWFile(meta)
	if err != nil {
		return nil, err
	}
	return []string{snapshot.SnapshotMetaFile, cowFile}, nil
}
