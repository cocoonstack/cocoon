package hypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// PrepareCloneDataDisks creates the clone's --data-disk files; the pre-create scan exists because a name colliding with an inherited serial would overwrite its backing file.
func (b *Backend) PrepareCloneDataDisks(ctx context.Context, vmID string, vmCfg *types.VMConfig, existing []*types.StorageConfig) ([]*types.StorageConfig, error) {
	for _, spec := range vmCfg.DataDisks {
		if slices.ContainsFunc(existing, func(sc *types.StorageConfig) bool { return sc.Serial == spec.Name }) {
			return nil, fmt.Errorf("--data-disk name %q collides with a disk inherited from the snapshot", spec.Name)
		}
	}
	return PrepareDataDisks(ctx, b.Conf.VMRunDir(vmID), vmCfg.DataDisks)
}

// PrepareOCIDisks creates the ext4-formatted sparse COW and the data disks, and returns storageConfigs with both appended.
func (b *Backend) PrepareOCIDisks(ctx context.Context, vmID string, vmCfg *types.VMConfig, storageConfigs []*types.StorageConfig) ([]*types.StorageConfig, error) {
	runDir := b.Conf.VMRunDir(vmID)
	cowPath := filepath.Join(runDir, types.COWRawFileName)
	if err := createSparseFile(cowPath, vmCfg.Storage); err != nil {
		return nil, err
	}
	if err := InitCOWFilesystem(ctx, cowPath); err != nil {
		return nil, err
	}
	dataDisks, err := PrepareDataDisks(ctx, runDir, vmCfg.DataDisks)
	if err != nil {
		return nil, err
	}
	storageConfigs = append(storageConfigs, &types.StorageConfig{Path: cowPath, Serial: CowSerial, Role: types.StorageRoleCOW})
	return append(storageConfigs, dataDisks...), nil
}

func InitCOWFilesystem(ctx context.Context, path string) error {
	out, err := exec.CommandContext( //nolint:gosec
		ctx,
		"mkfs.ext4", "-F", "-m", "0", "-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,discard",
		path,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// DataDiskBaseName returns the canonical data disk file name.
func DataDiskBaseName(serial string) string {
	return "data-" + serial + ".raw"
}

func IsDataDiskFile(name string) bool {
	return strings.HasPrefix(name, "data-") && strings.HasSuffix(name, ".raw")
}

// DiskPathByRole returns the recorded path of the first disk with the given role, or "".
func DiskPathByRole(configs []*types.StorageConfig, role types.StorageRole) string {
	for _, sc := range configs {
		if sc.Role == role {
			return sc.Path
		}
	}
	return ""
}

// RecordedCOWPath reads the recorded path, not a conf-derived one: after a --run-dir change the disks stay where the record says.
func RecordedCOWPath(rec *VMRecord) (string, error) {
	cowPath := DiskPathByRole(rec.StorageConfigs, types.StorageRoleCOW)
	if cowPath == "" {
		return "", fmt.Errorf("no COW disk recorded for %s", rec.ID)
	}
	return cowPath, nil
}

// CopyWritableDisks copies concurrently; durability is paid at persist.
func CopyWritableDisks(ctx context.Context, dstDir string, rec *VMRecord) error {
	pairs, err := writableDiskPairs(dstDir, rec)
	if err != nil {
		return err
	}
	return copyPairs(ctx, pairs)
}

// PrepareDataDisks requires unique, ValidDataDiskName-passing names.
func PrepareDataDisks(ctx context.Context, baseDir string, specs []types.DataDiskSpec) ([]*types.StorageConfig, error) {
	out := make([]*types.StorageConfig, 0, len(specs))
	for _, spec := range specs {
		out = append(out, &types.StorageConfig{
			Path:       filepath.Join(baseDir, DataDiskBaseName(spec.Name)),
			Serial:     spec.Name,
			Role:       types.StorageRoleData,
			MountPoint: spec.MountPoint,
			FSType:     spec.FSType,
			DirectIO:   spec.DirectIO,
		})
	}
	if _, err := utils.Map(ctx, specs, func(ctx context.Context, i int, spec types.DataDiskSpec) (struct{}, error) {
		if err := createSparseFile(out[i].Path, spec.Size); err != nil {
			return struct{}{}, fmt.Errorf("data disk %s: %w", spec.Name, err)
		}
		if spec.FSType == types.FSTypeExt4 {
			if err := InitCOWFilesystem(ctx, out[i].Path); err != nil {
				return struct{}{}, fmt.Errorf("data disk %s: mkfs: %w", spec.Name, err)
			}
		}
		return struct{}{}, nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// writableDiskPairs returns {copy under dstDir, live disk} for the COW and every data disk.
func writableDiskPairs(dstDir string, rec *VMRecord) ([][2]string, error) {
	cowPath, err := RecordedCOWPath(rec)
	if err != nil {
		return nil, err
	}
	pairs := [][2]string{{filepath.Join(dstDir, filepath.Base(cowPath)), cowPath}}
	for _, sc := range rec.StorageConfigs {
		if sc.Role == types.StorageRoleData {
			pairs = append(pairs, [2]string{filepath.Join(dstDir, DataDiskBaseName(sc.Serial)), sc.Path})
		}
	}
	return pairs, nil
}

func copyPairs(ctx context.Context, pairs [][2]string) error {
	_, err := utils.Map(ctx, pairs, func(ctx context.Context, _ int, p [2]string) (struct{}, error) {
		if err := utils.ReflinkCopy(ctx, p[0], p[1], utils.NoSync); err != nil {
			return struct{}{}, fmt.Errorf("copy %s: %w", filepath.Base(p[1]), err)
		}
		return struct{}{}, nil
	})
	return err
}

// createSparseFile creates path truncated to size; os.Truncate alone won't create a missing file.
func createSparseFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	truncErr := f.Truncate(size)
	closeErr := f.Close()
	if truncErr != nil {
		return fmt.Errorf("truncate %s: %w", path, truncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}
	return nil
}
