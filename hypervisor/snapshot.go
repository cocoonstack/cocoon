package hypervisor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// SnapshotMetaFile is the cocoon-owned sidecar carrying fields the hypervisor's native config can't hold (Role/MountPoint/FSType/DirectIO; FC CPU/Memory).
const SnapshotMetaFile = "cocoon.json"

type SnapshotMeta struct {
	StorageConfigs []*types.StorageConfig `json:"storage_configs"`
	BootConfig     *types.BootConfig      `json:"boot_config,omitempty"`
	// CPU/Memory populated by FC only; CH reads them from config.json on restore.
	CPU    int   `json:"cpu,omitempty"`
	Memory int64 `json:"memory,omitempty"`
}

// RecordSnapshot generates a snapshot ID and records it on the VM's record.
func (b *Backend) RecordSnapshot(ctx context.Context, vmID string) (string, error) {
	snapID := utils.GenerateID()
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		r, err := idx.GetRecord(vmID)
		if err != nil {
			return err
		}
		if r.SnapshotIDs == nil {
			r.SnapshotIDs = make(map[string]struct{})
		}
		r.SnapshotIDs[snapID] = struct{}{}
		return nil
	}); err != nil {
		return "", fmt.Errorf("record snapshot on VM: %w", err)
	}
	return snapID, nil
}

func (b *Backend) BuildSnapshotConfig(snapID string, rec *VMRecord) *types.SnapshotConfig {
	cfg := &types.SnapshotConfig{
		ID:           snapID,
		Hypervisor:   b.Typ,
		NICs:         len(rec.NetworkConfigs),
		ImageBlobIDs: maps.Clone(rec.ImageBlobIDs),
		Config:       rec.Config.Config,
	}
	return cfg
}

// SnapshotSequence is the shared capture skeleton; only capture runs in the pause window — AfterCapture (e.g. cidata copy) runs outside.
func (b *Backend) SnapshotSequence(ctx context.Context, ref string, spec SnapshotSpec) (_ *types.SnapshotConfig, _ io.ReadCloser, err error) {
	vmID, err := b.ResolveRef(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	rec, err := b.LoadRecord(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}
	if vErr := types.ValidateStorageConfigs(rec.StorageConfigs); vErr != nil {
		return nil, nil, fmt.Errorf("storage invariants violated: %w", vErr)
	}

	tmpDir, err := os.MkdirTemp(b.Conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		}
	}()

	hc := utils.NewSocketHTTPClient(SocketPath(rec.RunDir))
	pause := func() error { return spec.Pause(&rec, hc) }
	resume := func() error { return spec.Resume(&rec, hc) }
	captureWindow := func() error {
		return b.WithPausedVM(ctx, &rec, pause, resume, func() error {
			return spec.Capture(&rec, hc, tmpDir)
		})
	}
	if spec.Wrap != nil {
		err = spec.Wrap(&rec, captureWindow)
	} else {
		err = captureWindow()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}

	if spec.AfterCapture != nil {
		if err = spec.AfterCapture(&rec, tmpDir); err != nil {
			return nil, nil, err
		}
	}

	meta, err := spec.BuildMeta(&rec, tmpDir)
	if err != nil {
		return nil, nil, fmt.Errorf("build snapshot metadata: %w", err)
	}
	if err = SaveSnapshotMeta(tmpDir, meta); err != nil {
		return nil, nil, fmt.Errorf("save snapshot metadata: %w", err)
	}

	snapID, err := b.RecordSnapshot(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}
	return b.BuildSnapshotConfig(snapID, &rec), utils.TarDirStreamWithRemove(tmpDir), nil
}

func SaveSnapshotMeta(dir string, meta *SnapshotMeta) error {
	return utils.AtomicWriteJSON(filepath.Join(dir, SnapshotMetaFile), meta)
}

func LoadSnapshotMeta(dir string) (*SnapshotMeta, error) {
	var meta SnapshotMeta
	if err := utils.ReadJSONFile(filepath.Join(dir, SnapshotMetaFile), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func LoadAndValidateMeta(dir, rootDir, runDir string) (*SnapshotMeta, error) {
	meta, err := LoadSnapshotMeta(dir)
	if err != nil {
		return nil, err
	}
	if err := ValidateMetaPaths(meta, rootDir, runDir); err != nil {
		return nil, err
	}
	return meta, nil
}

// PopulateFromSrc cleans runDir of old snapshot files then copies fresh ones from srcDir (used by DirectRestore).
func PopulateFromSrc(runDir, srcDir string, clean func(string) error, clone func(string, string) error) error {
	if err := clean(runDir); err != nil {
		return fmt.Errorf("clean old snapshot files: %w", err)
	}
	if err := clone(runDir, srcDir); err != nil {
		return fmt.Errorf("clone snapshot files: %w", err)
	}
	return nil
}

// PreflightRestore: load+validate sidecar, run backend-specific integrity, assert snapshot role sequence is a prefix of rec.
func PreflightRestore(srcDir, rootDir, runDir string, rec *VMRecord, integrity func(srcDir string, sidecar []*types.StorageConfig) error) error {
	meta, err := LoadAndValidateMeta(srcDir, rootDir, runDir)
	if err != nil {
		return err
	}
	if err := integrity(srcDir, meta.StorageConfigs); err != nil {
		return err
	}
	return ValidateRoleSequence(meta.StorageConfigs, rec.StorageConfigs)
}

func CloneStorageConfigs(storageConfigs []*types.StorageConfig) []*types.StorageConfig {
	out := make([]*types.StorageConfig, 0, len(storageConfigs))
	for _, sc := range storageConfigs {
		if sc == nil {
			continue
		}
		cp := *sc
		out = append(out, &cp)
	}
	return out
}

// IsUnderDir reports whether path is strictly under dir. An empty dir returns false (disables the check) rather than matching every path.
func IsUnderDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	root := filepath.Clean(dir)
	return strings.HasPrefix(cleaned, root+string(filepath.Separator))
}

// ValidateMetaPaths rejects sidecar paths escaping cocoon-managed roots; an imported snapshot's cocoon.json is otherwise untrusted.
func ValidateMetaPaths(meta *SnapshotMeta, rootDir, runDir string) error {
	for _, sc := range meta.StorageConfigs {
		if !IsUnderDir(sc.Path, rootDir) && !IsUnderDir(sc.Path, runDir) {
			return fmt.Errorf("untrusted storage path in snapshot metadata: %s", sc.Path)
		}
	}
	if b := meta.BootConfig; b != nil {
		if b.KernelPath != "" && !IsUnderDir(b.KernelPath, rootDir) {
			return fmt.Errorf("untrusted kernel path in snapshot metadata: %s", b.KernelPath)
		}
		if b.InitrdPath != "" && !IsUnderDir(b.InitrdPath, rootDir) {
			return fmt.Errorf("untrusted initrd path in snapshot metadata: %s", b.InitrdPath)
		}
	}
	return nil
}

// ReverseLayers projects Role==Layer entries through fn in reverse order (topmost layer first, matching overlayfs lowerdir semantics).
func ReverseLayers[T any](storageConfigs []*types.StorageConfig, project func(idx int, sc *types.StorageConfig) T) []T {
	var layers []*types.StorageConfig
	for _, sc := range storageConfigs {
		if sc.Role == types.StorageRoleLayer {
			layers = append(layers, sc)
		}
	}
	out := make([]T, len(layers))
	for i, sc := range layers {
		out[len(layers)-1-i] = project(i, sc)
	}
	return out
}
