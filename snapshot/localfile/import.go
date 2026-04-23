package localfile

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Import reads a tar archive (optionally gzip-compressed) containing snapshot.json
// metadata and data files, stores the snapshot, and returns the new snapshot ID.
// Gzip wrapping is auto-detected from magic bytes; raw tar is accepted directly.
// Non-empty name and description override values from snapshot.json.
func (lf *LocalFile) Import(ctx context.Context, r io.Reader, name, description string) (_ string, err error) {
	tarReader, gzCloser, err := unwrapGzip(r)
	if err != nil {
		return "", err
	}

	id := utils.GenerateID()
	dataDir := lf.conf.SnapshotDataDir(id)

	if err = os.MkdirAll(dataDir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dataDir) //nolint:errcheck,gosec
		}
	}()

	if err = utils.ExtractTar(dataDir, tarReader); err != nil {
		return "", fmt.Errorf("extract archive: %w", err)
	}
	// Verify gzip checksum (CRC32 + size) when the input was gzip-wrapped.
	if gzCloser != nil {
		if err = gzCloser.Close(); err != nil {
			return "", fmt.Errorf("gzip integrity check: %w", err)
		}
	}

	cfg, err := readAndRemoveSnapshotJSON(dataDir)
	if err != nil {
		return "", err
	}

	cfg.ID = id
	if name != "" {
		cfg.Name = name
	}
	if description != "" {
		cfg.Description = description
	}

	if err = lf.store.Update(ctx, func(idx *snapshot.SnapshotIndex) error {
		if cfg.Name != "" {
			if existingID, ok := idx.Names[cfg.Name]; ok {
				return fmt.Errorf("snapshot name %q already in use by %s", cfg.Name, existingID)
			}
		}
		idx.Snapshots[id] = &snapshot.SnapshotRecord{
			Snapshot: types.Snapshot{
				SnapshotConfig: *cfg,
				CreatedAt:      time.Now(),
			},
			DataDir: dataDir,
		}
		if cfg.Name != "" {
			idx.Names[cfg.Name] = id
		}
		return nil
	}); err != nil {
		return "", err
	}

	return id, nil
}

// unwrapGzip peeks at the first 2 bytes to detect gzip magic (0x1f 0x8b).
// Returns the underlying tar reader and an optional gzip closer.
func unwrapGzip(r io.Reader) (io.Reader, io.Closer, error) {
	br := bufio.NewReaderSize(r, 4096)
	peek, err := br.Peek(2)
	if err != nil {
		return nil, nil, fmt.Errorf("peek archive header: %w", err)
	}
	if peek[0] == 0x1f && peek[1] == 0x8b {
		gr, gzErr := gzip.NewReader(br)
		if gzErr != nil {
			return nil, nil, fmt.Errorf("decompress: %w", gzErr)
		}
		return gr, gr, nil
	}
	return br, nil, nil
}

// readAndRemoveSnapshotJSON reads snapshot.json from the data directory,
// parses the SnapshotExport envelope, validates it, and removes the file.
func readAndRemoveSnapshotJSON(dataDir string) (*types.SnapshotConfig, error) {
	path := filepath.Join(dataDir, snapshotJSONName)
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("invalid snapshot archive: %s not found", snapshotJSONName)
		}
		return nil, fmt.Errorf("read %s: %w", snapshotJSONName, err)
	}

	var envelope types.SnapshotExport
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse %s: %w", snapshotJSONName, err)
	}
	if envelope.Version != 1 {
		return nil, fmt.Errorf("unsupported snapshot archive version %d", envelope.Version)
	}

	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("remove %s from data dir: %w", snapshotJSONName, err)
	}

	return &envelope.Config, nil
}
