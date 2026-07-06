package hypervisor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// SnapshotMetaFile is the cocoon-owned sidecar carrying fields the hypervisor's native config can't hold (Role/MountPoint/FSType/DirectIO; FC CPU/Memory).
const SnapshotMetaFile = "cocoon.json"

// SnapshotMeta is the schema of the cocoon.json snapshot sidecar.
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

// UnrecordSnapshot drops a snapshot id from the VM's record (persist-failure rollback); best-effort, a leftover id is cosmetic.
func (b *Backend) UnrecordSnapshot(ctx context.Context, vmID, snapID string) {
	if err := b.DB.Update(ctx, func(idx *VMIndex) error {
		r, err := idx.GetRecord(vmID)
		if err != nil {
			return err
		}
		delete(r.SnapshotIDs, snapID)
		return nil
	}); err != nil {
		log.WithFunc(b.Typ+".UnrecordSnapshot").Warnf(ctx, "unrecord snapshot %s on %s: %v", snapID, vmID, err)
	}
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
	vmID, rec, tmpDir, err := b.prepareSnapshot(ctx, ref)
	if err != nil {
		return nil, nil, err
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
	if err = runWrapped(&rec, spec.Wrap, captureWindow); err != nil {
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}
	cfg, err := b.finalizeSnapshot(ctx, vmID, &rec, spec, tmpDir)
	if err != nil {
		return nil, nil, err
	}
	return cfg, utils.TarDirStreamWithRemove(tmpDir), nil
}

// HibernateSequence is SnapshotSequence with persist inside the pause window and terminate instead of resume: the snapshot point and the stop coincide, any failure before terminate resumes the VM, and a failed terminate marks it error.
func (b *Backend) HibernateSequence(ctx context.Context, ref string, spec HibernateSpec, persist func(cfg *types.SnapshotConfig, stream io.ReadCloser) error) (err error) {
	vmID, rec, tmpDir, err := b.prepareSnapshot(ctx, ref)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		}
	}()

	logger := log.WithFunc(b.Typ + ".HibernateSequence")
	hc := utils.NewSocketHTTPClient(SocketPath(rec.RunDir))
	var killFailed bool
	window := func() error {
		return b.WithRunningVM(ctx, &rec, func(pid int) error {
			if pErr := spec.Pause(&rec, hc); pErr != nil {
				return fmt.Errorf("pause: %w", pErr)
			}
			failResume := func(e error) error {
				if rErr := spec.Resume(&rec, hc); rErr != nil {
					logger.Warnf(ctx, "resume VM %s after failed hibernate: %v", rec.ID, rErr)
				}
				return e
			}
			if cErr := spec.Capture(&rec, hc, tmpDir); cErr != nil {
				return failResume(cErr)
			}
			cfg, sErr := b.finalizeSnapshot(ctx, vmID, &rec, spec.SnapshotSpec, tmpDir)
			if sErr != nil {
				return failResume(sErr)
			}
			if pErr := persist(cfg, utils.TarDirStreamWithRemove(tmpDir)); pErr != nil {
				b.UnrecordSnapshot(ctx, vmID, cfg.ID)
				return failResume(fmt.Errorf("persist snapshot: %w", pErr))
			}
			if kErr := spec.Terminate(&rec, hc, pid); kErr != nil {
				killFailed = true
				return fmt.Errorf("terminate: %w", kErr)
			}
			return nil
		})
	}
	if err = runWrapped(&rec, spec.Wrap, window); err != nil {
		if killFailed {
			b.MarkError(ctx, vmID)
		}
		return fmt.Errorf("hibernate VM %s: %w", vmID, err)
	}

	CleanupRuntimeFiles(ctx, rec.RunDir, spec.RuntimeFiles)
	// Warn-and-continue like StopAll: the VMM is dead and the flip self-heals; the snapshot is already durable.
	if uErr := b.UpdateStates(ctx, []string{vmID}, types.VMStateStopped); uErr != nil {
		logger.Warnf(ctx, "mark stopped %s: %v", vmID, uErr)
	}
	return nil
}

// prepareSnapshot resolves ref and stages a capture dir inside the VM's run dir.
func (b *Backend) prepareSnapshot(ctx context.Context, ref string) (string, VMRecord, string, error) {
	vmID, err := b.ResolveRef(ctx, ref)
	if err != nil {
		return "", VMRecord{}, "", err
	}
	rec, err := b.LoadRecord(ctx, vmID)
	if err != nil {
		return "", VMRecord{}, "", err
	}
	if vErr := types.ValidateStorageConfigs(rec.StorageConfigs); vErr != nil {
		return "", VMRecord{}, "", fmt.Errorf("storage invariants violated: %w", vErr)
	}
	tmpDir, err := os.MkdirTemp(b.Conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return "", VMRecord{}, "", fmt.Errorf("create temp dir: %w", err)
	}
	return vmID, rec, tmpDir, nil
}

// finalizeSnapshot writes the sidecar metadata and registers the snapshot on the VM record.
func (b *Backend) finalizeSnapshot(ctx context.Context, vmID string, rec *VMRecord, spec SnapshotSpec, tmpDir string) (*types.SnapshotConfig, error) {
	if spec.AfterCapture != nil {
		if err := spec.AfterCapture(rec, tmpDir); err != nil {
			return nil, err
		}
	}
	meta, err := spec.BuildMeta(rec, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("build snapshot metadata: %w", err)
	}
	if err = SaveSnapshotMeta(tmpDir, meta); err != nil {
		return nil, fmt.Errorf("save snapshot metadata: %w", err)
	}
	snapID, err := b.RecordSnapshot(ctx, vmID)
	if err != nil {
		return nil, err
	}
	return b.BuildSnapshotConfig(snapID, rec), nil
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
