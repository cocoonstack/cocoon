package hypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func InitCOWFilesystem(ctx context.Context, path string) error {
	// shell out because no Go ext4 formatter library; mkfs.ext4 is authoritative.
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

// DataDiskBaseName is the canonical file name (centralized so matchers, reflink loops, and clone path rewrites stay in sync).
func DataDiskBaseName(serial string) string {
	return "data-" + serial + ".raw"
}

// IsDataDiskFile reports whether name matches the data disk file pattern.
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

// CopyWritableDisks reflinks the COW disk and every Role==Data disk into dstDir concurrently: inside the snapshot pause window, wall time is the longest single copy instead of the sum. Durability is paid at persist (SyncTree / store ingestion), not here.
func CopyWritableDisks(ctx context.Context, dstDir, cowPath string, configs []*types.StorageConfig) error {
	pairs := [][2]string{{filepath.Join(dstDir, filepath.Base(cowPath)), cowPath}}
	for _, sc := range configs {
		if sc.Role == types.StorageRoleData {
			pairs = append(pairs, [2]string{filepath.Join(dstDir, DataDiskBaseName(sc.Serial)), sc.Path})
		}
	}
	return copyPairs(ctx, pairs, utils.NoSync)
}

// PrepareDataDisks creates sparse files for each spec under baseDir, optionally formats (ext4 default), returns StorageConfigs; names must be unique and ValidDataDiskName-passing.
func PrepareDataDisks(ctx context.Context, baseDir string, specs []types.DataDiskSpec) ([]*types.StorageConfig, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(specs))
	out := make([]*types.StorageConfig, 0, len(specs))
	for i, spec := range specs {
		if !types.ValidDataDiskName(spec.Name) {
			return nil, fmt.Errorf("data disk %d: invalid name %q", i, spec.Name)
		}
		if _, dup := seen[spec.Name]; dup {
			return nil, fmt.Errorf("data disk: name %q duplicated", spec.Name)
		}
		seen[spec.Name] = struct{}{}
		if spec.Size < MinDataDiskSize {
			return nil, fmt.Errorf("data disk %s: size %d below %d minimum", spec.Name, spec.Size, MinDataDiskSize)
		}
		if spec.FSType != types.FSTypeExt4 && spec.FSType != types.FSTypeNone {
			return nil, fmt.Errorf("data disk %s: fstype %q not supported", spec.Name, spec.FSType)
		}
		out = append(out, &types.StorageConfig{
			Path:       filepath.Join(baseDir, DataDiskBaseName(spec.Name)),
			RO:         false,
			Serial:     spec.Name,
			Role:       types.StorageRoleData,
			MountPoint: spec.MountPoint,
			FSType:     spec.FSType,
			DirectIO:   spec.DirectIO,
		})
	}
	// Fan out sparse-create + mkfs like copyPairs: on the create path, wall time is the slowest disk, not the sum.
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

// PrepareOCICOW creates an ext4-formatted sparse COW at cowPath and returns storageConfigs with the new CowSerial entry appended (use the returned slice; append may reallocate).
func PrepareOCICOW(ctx context.Context, cowPath string, storage int64, storageConfigs []*types.StorageConfig) ([]*types.StorageConfig, error) {
	if err := createSparseFile(cowPath, storage); err != nil {
		return nil, err
	}
	if err := InitCOWFilesystem(ctx, cowPath); err != nil {
		return nil, err
	}
	return append(storageConfigs, &types.StorageConfig{
		Path:   cowPath,
		RO:     false,
		Serial: CowSerial,
		Role:   types.StorageRoleCOW,
	}), nil
}

func copyPairs(ctx context.Context, pairs [][2]string, sync utils.SyncMode) error {
	_, err := utils.Map(ctx, pairs, func(_ context.Context, _ int, p [2]string) (struct{}, error) {
		if err := utils.ReflinkCopy(ctx, p[0], p[1], sync); err != nil {
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
