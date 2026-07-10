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

// Export streams the snapshot as a raw tar (first entry: snapshot.json envelope; rest: data files).
func (lf *LocalFile) Export(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, false)
}

// ExportCompressed streams the snapshot as a gzip-compressed tar archive.
func (lf *LocalFile) ExportCompressed(ctx context.Context, ref string) (io.ReadCloser, error) {
	return lf.export(ctx, ref, true)
}

// ExportToDir reflinks snapshot data into dir + writes snapshot.json last so its presence is the all-data-ready marker for --from-dir.
func (lf *LocalFile) ExportToDir(ctx context.Context, ref, dir string) error {
	dataDir, cfg, err := lf.DataDir(ctx, ref)
	if err != nil {
		return err
	}
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
	// Fan out: snapshot dirs hold a few large files (memory, COW, data disks), so wall time is the longest copy, not the sum.
	if _, err = utils.Map(ctx, names, func(_ context.Context, _ int, name string) (struct{}, error) {
		if copyErr := utils.ReflinkCopy(filepath.Join(dir, name), filepath.Join(dataDir, name)); copyErr != nil {
			return struct{}{}, fmt.Errorf("copy %s: %w", name, copyErr)
		}
		return struct{}{}, nil
	}); err != nil {
		return err
	}
	if err = snapshot.WriteSnapshotEnvelope(dir, cfg); err != nil {
		return fmt.Errorf("write envelope: %w", err)
	}
	return nil
}

func (lf *LocalFile) export(ctx context.Context, ref string, compress bool) (io.ReadCloser, error) {
	dataDir, cfg, err := lf.DataDir(ctx, ref)
	if err != nil {
		return nil, err
	}

	jsonData, err := snapshot.MarshalEnvelope(cfg)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var streamErr error
		defer func() {
			if streamErr != nil {
				pw.CloseWithError(streamErr) //nolint:errcheck,gosec
			} else {
				pw.Close() //nolint:errcheck,gosec
			}
			done <- streamErr
		}()

		var w io.Writer = pw
		var gw *gzip.Writer
		if compress {
			var gzErr error
			gw, gzErr = gzip.NewWriterLevel(pw, gzip.BestSpeed)
			if gzErr != nil {
				streamErr = fmt.Errorf("create gzip writer: %w", gzErr)
				return
			}
			w = gw
		}
		tw := tar.NewWriter(w)

		streamErr = tw.WriteHeader(&tar.Header{
			Name:    snapshot.SnapshotJSONName,
			Size:    int64(len(jsonData)),
			Mode:    0o644,
			ModTime: time.Now(),
		})
		if streamErr != nil {
			return
		}
		if _, streamErr = tw.Write(jsonData); streamErr != nil {
			return
		}

		if streamErr = utils.TarDir(tw, dataDir); streamErr != nil {
			return
		}

		if streamErr = tw.Close(); streamErr != nil {
			return
		}
		if gw != nil {
			streamErr = gw.Close()
		}
	}()

	return utils.NewPipeStreamReader(pr, done, nil), nil
}
