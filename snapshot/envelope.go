package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// ReadSnapshotEnvelope reads <dir>/snapshot.json into a SnapshotConfig.
func ReadSnapshotEnvelope(dir string) (types.SnapshotConfig, error) {
	path := filepath.Join(dir, SnapshotJSONName)
	envelope := types.SnapshotExport{}
	if err := utils.ReadJSONFile(path, &envelope); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return types.SnapshotConfig{}, fmt.Errorf("%s missing in %s: %w", SnapshotJSONName, dir, ErrEnvelopeMissing)
		}
		return types.SnapshotConfig{}, err
	}
	if envelope.Version != EnvelopeVersion {
		return types.SnapshotConfig{}, fmt.Errorf("unsupported snapshot envelope version %d (want %d)", envelope.Version, EnvelopeVersion)
	}
	return envelope.Config, nil
}

// MarshalEnvelope returns the indented snapshot.json bytes for cfg.
func MarshalEnvelope(cfg types.SnapshotConfig) ([]byte, error) {
	data, err := json.MarshalIndent(types.SnapshotExport{Version: EnvelopeVersion, Config: cfg}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot envelope: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteSnapshotEnvelope writes <dir>/snapshot.json atomically so a concurrent reader can't see a partial write.
func WriteSnapshotEnvelope(dir string, cfg types.SnapshotConfig) error {
	data, err := MarshalEnvelope(cfg)
	if err != nil {
		return err
	}
	return utils.AtomicWriteFile(filepath.Join(dir, SnapshotJSONName), data, 0o644, utils.Sync)
}
