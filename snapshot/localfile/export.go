package localfile

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/utils"
)

func (lf *LocalFile) Export(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, false)
}

// ExportCompressed streams the snapshot as a gzip-compressed tar archive.
func (lf *LocalFile) ExportCompressed(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, true)
}

// ExportToDir reflinks snapshot data into dir + writes snapshot.json last so its presence is the all-data-ready marker for --from-dir.
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
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read snapshot dir: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	// a partial tree would make the retry refuse the non-empty target
	defer func() {
		if err != nil {
			for _, name := range names {
				os.Remove(filepath.Join(dir, name)) //nolint:errcheck,gosec
			}
		}
	}()
	// Fan out: snapshot dirs hold a few large files (memory, COW, data disks), so wall time is the longest copy, not the sum.
	if _, err = utils.Map(ctx, names, func(ctx context.Context, _ int, name string) (struct{}, error) {
		if copyErr := utils.ReflinkCopy(ctx, filepath.Join(dir, name), filepath.Join(dataDir, name), utils.Sync); copyErr != nil {
			return struct{}{}, fmt.Errorf("copy %s: %w", name, copyErr)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err = snapshot.WriteSnapshotEnvelope(dir, cfg, names); err != nil {
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
	jsonData, err := snapshot.MarshalEnvelope(cfg, files)
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
