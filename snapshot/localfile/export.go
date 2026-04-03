package localfile

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const snapshotJSONName = "snapshot.json"

// Export streams the snapshot as a gzip-compressed tar archive.
// The first tar entry is snapshot.json containing the SnapshotConfig metadata;
// the remaining entries are the data files from the snapshot directory.
func (lf *LocalFile) Export(ctx context.Context, ref string) (io.ReadCloser, error) {
	dataDir, cfg, err := lf.DataDir(ctx, ref)
	if err != nil {
		return nil, err
	}

	envelope := types.SnapshotExport{Version: 1, Config: *cfg}
	jsonData, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	jsonData = append(jsonData, '\n')

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

		gw, err := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		if err != nil {
			streamErr = fmt.Errorf("create gzip writer: %w", err)
			return
		}
		tw := tar.NewWriter(gw)

		// First entry: snapshot.json metadata.
		streamErr = tw.WriteHeader(&tar.Header{
			Name:    snapshotJSONName,
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

		// Remaining entries: data files from the snapshot directory.
		if streamErr = utils.TarDir(tw, dataDir); streamErr != nil {
			return
		}

		if streamErr = tw.Close(); streamErr != nil {
			return
		}
		streamErr = gw.Close()
	}()

	return utils.NewPipeStreamReader(pr, done, nil), nil
}
