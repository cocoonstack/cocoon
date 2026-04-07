package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	snapshotVMStateFile = "vmstate"
	snapshotMemFile     = "mem"
	snapshotMetaFile    = "cocoon.json"
)

// Snapshot pauses the VM, captures its full state (CPU/device state via FC
// snapshot API + memory file + COW disk via reflink copy), resumes the VM,
// and returns a streaming tar.gz reader of the snapshot directory.
func (fc *Firecracker) Snapshot(ctx context.Context, ref string) (*types.SnapshotConfig, io.ReadCloser, error) {
	logger := log.WithFunc("firecracker.Snapshot")

	vmID, err := fc.ResolveRef(ctx, ref)
	if err != nil {
		return nil, nil, err
	}

	rec, err := fc.LoadRecord(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}

	sockPath := hypervisor.SocketPath(rec.RunDir)
	hc := utils.NewSocketHTTPClient(sockPath)

	cowPath := fc.conf.COWRawPath(vmID)

	// Serialize with concurrent clone redirects that may symlink cowPath.
	cowUnlock, cowLockErr := lockCOWPath(cowPath)
	if cowLockErr != nil {
		return nil, nil, fmt.Errorf("lock COW: %w", cowLockErr)
	}
	defer cowUnlock()

	// Create a temporary directory for the snapshot data.
	tmpDir, err := os.MkdirTemp(fc.conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}

	// withRunningVM verifies the process is alive, then runs the callback.
	// Inside the callback: pause -> FC snapshot -> ReflinkCopy COW -> resume.
	if err := fc.WithRunningVM(ctx, &rec, func(_ int) error {
		if err := pauseVM(ctx, hc); err != nil {
			return fmt.Errorf("pause: %w", err)
		}

		resumed := false
		var resumeErr error
		doResume := func() {
			if resumed {
				return
			}
			resumed = true
			resumeErr = resumeVM(context.WithoutCancel(ctx), hc)
			if resumeErr != nil {
				logger.Warnf(ctx, "resume VM %s: %v", vmID, resumeErr)
			}
		}
		defer doResume()

		if err := createSnapshotFC(ctx, hc, tmpDir); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}

		if err := utils.ReflinkCopy(filepath.Join(tmpDir, cowFileName), cowPath); err != nil {
			return fmt.Errorf("copy COW: %w", err)
		}

		// Resume eagerly so we can propagate the error.
		// The deferred doResume is a no-op when resumed=true.
		doResume()
		if resumeErr != nil {
			return fmt.Errorf("snapshot data captured but resume failed: %w", resumeErr)
		}
		return nil
	}); err != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}

	// Save snapshot metadata so clones can reconstruct storage/boot config
	// without depending on live VM records.
	if metaErr := saveSnapshotMeta(tmpDir, fc.conf.RootDir, rec.StorageConfigs, rec.BootConfig, rec.Config.CPU, rec.Config.Memory); metaErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("save snapshot metadata: %w", metaErr)
	}

	// Generate snapshot ID and record it on the VM atomically.
	snapID, genErr := utils.GenerateID()
	if genErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("generate snapshot ID: %w", genErr)
	}
	if updateErr := fc.DB.Update(ctx, func(idx *hypervisor.VMIndex) error {
		r := idx.VMs[vmID]
		if r == nil {
			return fmt.Errorf("vm %s disappeared from index", vmID)
		}
		if r.SnapshotIDs == nil {
			r.SnapshotIDs = make(map[string]struct{})
		}
		r.SnapshotIDs[snapID] = struct{}{}
		return nil
	}); updateErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("record snapshot on VM: %w", updateErr)
	}

	// Build SnapshotConfig from the VM record.
	cfg := &types.SnapshotConfig{
		ID:         snapID,
		Image:      rec.Config.Image,
		Hypervisor: typ,
		CPU:        rec.Config.CPU,
		Memory:     rec.Config.Memory,
		Storage:    rec.Config.Storage,
		NICs:       len(rec.NetworkConfigs),
		Network:    rec.Config.Network,
	}
	if rec.ImageBlobIDs != nil {
		cfg.ImageBlobIDs = make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(cfg.ImageBlobIDs, rec.ImageBlobIDs)
	}

	return cfg, utils.TarDirStreamWithRemove(tmpDir), nil
}

// snapshotMeta is persisted as cocoon.json inside the snapshot tar.
// It makes the snapshot self-contained: clones can reconstruct storage/boot
// config without depending on live VM records or image backends.
//
// Paths in StorageConfigs and BootConfig are stored RELATIVE to SourceRootDir.
// After loadSnapshotMeta, StorageConfigs/BootConfig are resolved against the
// LOCAL rootDir. vmstatePaths() reconstructs the original absolute paths
// (against SourceRootDir) for snapshot/load drive redirects.
type snapshotMeta struct {
	SourceRootDir  string                 `json:"source_root_dir"`
	StorageConfigs []*types.StorageConfig `json:"storage_configs"`       // resolved to local absolute paths after load
	BootConfig     *types.BootConfig      `json:"boot_config,omitempty"` // resolved to local absolute paths after load
	CPU            int                    `json:"cpu,omitempty"`         // snapshot's original vCPU count
	Memory         int64                  `json:"memory,omitempty"`      // snapshot's original memory (bytes)

	// rawRelPaths preserves the original relative paths from cocoon.json
	// so vmstatePaths() can reconstruct source-host absolute paths.
	rawRelPaths []string // populated by loadSnapshotMeta, not serialized
}

// saveSnapshotMeta stores paths relative to rootDir so snapshots are portable
// across hosts with different root_dir settings. Also normalizes vmlinux → vmlinuz
// for the kernel path since vmlinux is a host-local FC cache.
func saveSnapshotMeta(dir, rootDir string, storageConfigs []*types.StorageConfig, boot *types.BootConfig, cpu int, memory int64) error {
	meta := snapshotMeta{SourceRootDir: rootDir, CPU: cpu, Memory: memory}
	// Store ALL drive entries (RO layers + RW COW) so clones can:
	// 1. Reconstruct layer paths on the target host (RO entries)
	// 2. Know the source COW path for drive redirect/symlink (RW entry)
	for _, sc := range storageConfigs {
		meta.StorageConfigs = append(meta.StorageConfigs, &types.StorageConfig{
			Path: makeRelative(sc.Path, rootDir), RO: sc.RO, Serial: sc.Serial,
		})
	}
	if boot != nil {
		b := *boot
		// Store vmlinuz (portable), not vmlinux (FC-specific cache).
		if filepath.Base(b.KernelPath) == "vmlinux" {
			b.KernelPath = filepath.Join(filepath.Dir(b.KernelPath), "vmlinuz")
		}
		b.KernelPath = makeRelative(b.KernelPath, rootDir)
		if b.InitrdPath != "" {
			b.InitrdPath = makeRelative(b.InitrdPath, rootDir)
		}
		b.Cmdline = "" // cmdline is rebuilt on clone with new VM name/IP
		meta.BootConfig = &b
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, snapshotMetaFile), data, 0o600)
}

// makeRelative strips the rootDir prefix from an absolute path.
// Returns the path unchanged if it doesn't start with rootDir.
func makeRelative(absPath, rootDir string) string {
	rel, err := filepath.Rel(rootDir, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

// loadSnapshotMeta reads cocoon.json. StorageConfigs and BootConfig paths
// are resolved against localRootDir for actual file access. The original
// vmstate paths (resolved against SourceRootDir) are available via
// vmstatePaths() for creating drive redirects.
func loadSnapshotMeta(dir, localRootDir string) (*snapshotMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotMetaFile)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", snapshotMetaFile, err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode %s: %w", snapshotMetaFile, err)
	}
	// Save raw relative paths before resolving, so vmstatePaths() can
	// reconstruct the source-host absolute paths independently.
	meta.rawRelPaths = make([]string, len(meta.StorageConfigs))
	for i, sc := range meta.StorageConfigs {
		meta.rawRelPaths[i] = sc.Path
	}
	// Resolve relative paths against the LOCAL rootDir for file access.
	for _, sc := range meta.StorageConfigs {
		if !filepath.IsAbs(sc.Path) {
			sc.Path = filepath.Join(localRootDir, sc.Path)
		}
	}
	if b := meta.BootConfig; b != nil {
		if b.KernelPath != "" && !filepath.IsAbs(b.KernelPath) {
			b.KernelPath = filepath.Join(localRootDir, b.KernelPath)
		}
		if b.InitrdPath != "" && !filepath.IsAbs(b.InitrdPath) {
			b.InitrdPath = filepath.Join(localRootDir, b.InitrdPath)
		}
	}
	return &meta, nil
}

// vmstatePaths reconstructs the absolute paths that FC's vmstate binary
// has baked in (source host paths). Uses the raw relative paths from
// cocoon.json resolved against SourceRootDir, NOT the locally-resolved paths.
func (m *snapshotMeta) vmstatePaths() []*types.StorageConfig {
	if m.SourceRootDir == "" || len(m.rawRelPaths) == 0 {
		return m.StorageConfigs // legacy: no source info, use as-is
	}
	configs := make([]*types.StorageConfig, len(m.StorageConfigs))
	for i, sc := range m.StorageConfigs {
		path := m.rawRelPaths[i]
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.SourceRootDir, path)
		}
		configs[i] = &types.StorageConfig{Path: path, RO: sc.RO, Serial: sc.Serial}
	}
	return configs
}
