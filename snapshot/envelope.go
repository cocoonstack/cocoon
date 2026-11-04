package snapshot

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	SnapshotJSONName = "snapshot.json"
	EnvelopeVersion  = 1

	// SnapshotMetaFile is the cocoon-owned sidecar carrying fields the hypervisor's native config cannot hold.
	SnapshotMetaFile = "cocoon.json"
)

// ErrEnvelopeMissing wraps the not-found case so callers can render a dir-specific error instead of a raw open failure.
var ErrEnvelopeMissing = errors.New("snapshot envelope missing")

// SnapshotMeta is the schema of the cocoon.json snapshot sidecar.
type SnapshotMeta struct {
	StorageConfigs []*types.StorageConfig `json:"storage_configs"`
	BootConfig     *types.BootConfig      `json:"boot_config,omitzero"`
	NICMACs        []string               `json:"nic_macs,omitzero"`
}

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

// MarshalEnvelope returns the indented snapshot.json bytes for cfg, the files exported beside it, and the subset a disk attach reads.
func MarshalEnvelope(cfg types.SnapshotConfig, files, diskAttachFiles []string) ([]byte, error) {
	data, err := json.Marshal(types.SnapshotExport{Version: EnvelopeVersion, Config: cfg, Files: files, DiskAttachFiles: diskAttachFiles}, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot envelope: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteSnapshotEnvelope writes <dir>/snapshot.json atomically so a concurrent reader can't see a partial write.
func WriteSnapshotEnvelope(dir string, cfg types.SnapshotConfig, files, diskAttachFiles []string) error {
	data, err := MarshalEnvelope(cfg, files, diskAttachFiles)
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

func SaveSnapshotMeta(dir string, meta *SnapshotMeta) error {
	return utils.AtomicWriteJSON(filepath.Join(dir, SnapshotMetaFile), meta, utils.Sync)
}

func LoadSnapshotMeta(dir string) (*SnapshotMeta, error) {
	var meta SnapshotMeta
	if err := utils.ReadJSONFile(filepath.Join(dir, SnapshotMetaFile), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// SnapshotCOWFile returns the basename of the sidecar's COW disk; only the basename is trusted, so another node's sidecar works.
func SnapshotCOWFile(meta *SnapshotMeta) (string, error) {
	i := slices.IndexFunc(meta.StorageConfigs, func(sc *types.StorageConfig) bool { return sc != nil && sc.Role == types.StorageRoleCOW })
	if i < 0 {
		return "", fmt.Errorf("no cow disk in snapshot metadata")
	}
	return filepath.Base(meta.StorageConfigs[i].Path), nil
}
