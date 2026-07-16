package localfile

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// Import reads a snapshot tar (gzip auto-detected), stores it, returns the new ID. Non-empty name/description override the envelope.
func (lf *LocalFile) Import(ctx context.Context, r io.Reader, name, description string) (_ string, err error) {
	tarReader, gzCloser, err := unwrapGzip(r)
	if err != nil {
		return "", err
	}

	id := utils.GenerateID()
	dataDir := lf.conf.SnapshotDataDir(id)

	// Import writes the data dir before any DB record exists; the build lease keeps GC's orphan sweep off it.
	release, err := lf.acquireBuildLease(id)
	if err != nil {
		return "", err
	}
	defer release()

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
	// Drain to true EOF before Close: tar stops at its end-of-archive blocks,
	// and gzip only verifies the CRC32/size trailer once the stream is read out.
	if gzCloser != nil {
		if _, err = io.Copy(io.Discard, tarReader); err != nil {
			return "", fmt.Errorf("gzip integrity check: %w", err)
		}
		if err = gzCloser.Close(); err != nil {
			return "", fmt.Errorf("gzip integrity check: %w", err)
		}
	}

	cfg, err := readAndRemoveSnapshotJSON(dataDir)
	if err != nil {
		return "", err
	}

	cfg.ID = id
	cfg.Name = cmp.Or(name, cfg.Name)
	cfg.Description = cmp.Or(description, cfg.Description)

	if err = cfg.Validate(); err != nil {
		return "", err
	}

	size, sizeErr := utils.DirSize(dataDir)
	if sizeErr != nil {
		return "", fmt.Errorf("compute data dir size: %w", sizeErr)
	}
	now := time.Now()
	if err = lf.insertRecord(ctx, id, cfg.Name, &snapshot.SnapshotRecord{
		Snapshot:       types.Snapshot{SnapshotConfig: cfg, CreatedAt: now},
		DataDir:        dataDir,
		SizeBytes:      size,
		LastAccessedAt: now,
	}); err != nil {
		return "", err
	}

	emitSnapStart(ctx, lf.metering, id, cfg.Hypervisor, size, now)
	return id, nil
}

// unwrapGzip sniffs the 2-byte gzip magic (0x1f 0x8b) and returns the tar reader plus a gzip closer (nil for raw tar).
func unwrapGzip(r io.Reader) (io.Reader, io.Closer, error) {
	head, full, err := utils.PeekReader(r, 2)
	if err != nil {
		return nil, nil, fmt.Errorf("peek archive header: %w", err)
	}
	if len(head) < 2 {
		return nil, nil, errors.New("peek archive header: stream shorter than gzip magic (2 bytes)")
	}
	if bytes.HasPrefix(head, utils.GzipMagic) {
		gr, gzErr := gzip.NewReader(full)
		if gzErr != nil {
			return nil, nil, fmt.Errorf("decompress: %w", gzErr)
		}
		return gr, gr, nil
	}
	return full, nil, nil
}

// readAndRemoveSnapshotJSON reads the envelope and deletes it; the registered snapshot dir keeps only runtime sidecars (cocoon.json), not import metadata.
func readAndRemoveSnapshotJSON(dataDir string) (types.SnapshotConfig, error) {
	cfg, err := snapshot.ReadSnapshotEnvelope(dataDir)
	if err != nil {
		if errors.Is(err, snapshot.ErrEnvelopeMissing) {
			return types.SnapshotConfig{}, fmt.Errorf("invalid snapshot archive: %s not found", snapshot.SnapshotJSONName)
		}
		return types.SnapshotConfig{}, err
	}
	path := filepath.Join(dataDir, snapshot.SnapshotJSONName)
	if err := os.Remove(path); err != nil {
		return types.SnapshotConfig{}, fmt.Errorf("remove %s from data dir: %w", snapshot.SnapshotJSONName, err)
	}
	return cfg, nil
}
