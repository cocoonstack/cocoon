package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	SnapshotJSONName = "snapshot.json"
	EnvelopeVersion  = 1
)

// ErrEnvelopeMissing wraps the not-found case so callers can render a dir-specific error instead of a raw open failure.
var ErrEnvelopeMissing = errors.New("snapshot envelope missing")

// ReadSnapshotEnvelope reads <dir>/snapshot.json.
func ReadSnapshotEnvelope(dir string) (types.SnapshotExport, error) {
	path := filepath.Join(dir, SnapshotJSONName)
	envelope := types.SnapshotExport{}
	if err := utils.ReadJSONFile(path, &envelope); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return types.SnapshotExport{}, fmt.Errorf("%s missing in %s: %w", SnapshotJSONName, dir, ErrEnvelopeMissing)
		}
		return types.SnapshotExport{}, err
	}
	if envelope.Version != EnvelopeVersion {
		return types.SnapshotExport{}, fmt.Errorf("unsupported snapshot envelope version %d (want %d)", envelope.Version, EnvelopeVersion)
	}
	return envelope, nil
}

// MarshalEnvelope returns the indented snapshot.json bytes for cfg and the files exported beside it.
func MarshalEnvelope(cfg types.SnapshotConfig, files []string) ([]byte, error) {
	data, err := json.MarshalIndent(types.SnapshotExport{Version: EnvelopeVersion, Config: cfg, Files: files}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot envelope: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteSnapshotEnvelope writes <dir>/snapshot.json atomically so a concurrent reader can't see a partial write.
func WriteSnapshotEnvelope(dir string, cfg types.SnapshotConfig, files []string) error {
	data, err := MarshalEnvelope(cfg, files)
	if err != nil {
		return err
	}
	return utils.AtomicWriteFile(filepath.Join(dir, SnapshotJSONName), data, 0o644, utils.Sync)
}

// VerifyManifest checks that every file the envelope lists landed in dir; a tar cut at an entry boundary reads as complete, so the manifest is what catches it.
func VerifyManifest(dir string, files []string) error {
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("%s listed by the envelope is missing from %s: the archive ended early", name, dir)
		}
	}
	return nil
}

func VerifyCOWSize(dir string, cfg types.SnapshotConfig) error {
	st, err := os.Stat(filepath.Join(dir, types.COWRawFileName))
	if err != nil || cfg.Storage <= 0 {
		return nil
	}
	if st.Size() != cfg.Storage {
		return fmt.Errorf("%s in %s is %d bytes but the envelope records %d: a third-party tar drops cocoon's sparse records and rebuilds a short, shifted disk; use the export tar as cocoon wrote it, or `snapshot export --to-dir`",
			types.COWRawFileName, dir, st.Size(), cfg.Storage)
	}
	return nil
}
