package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

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

	tmpDir, err := os.MkdirTemp(fc.conf.VMRunDir(vmID), "snapshot-")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp dir: %w", err)
	}

	// Serialize the COW copy with concurrent clone redirects.
	if err := withCOWPathLocked(cowPath, func() error {
		return fc.WithRunningVM(ctx, &rec, func(_ int) error {
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

			if err := createSnapshotFC(ctx, sockPath, tmpDir); err != nil {
				return fmt.Errorf("snapshot: %w", err)
			}
			if err := utils.ReflinkCopy(filepath.Join(tmpDir, cowFileName), cowPath); err != nil {
				return fmt.Errorf("copy COW: %w", err)
			}

			doResume()
			if resumeErr != nil {
				return fmt.Errorf("snapshot data captured but resume failed: %w", resumeErr)
			}
			return nil
		})
	}); err != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("snapshot VM %s: %w", vmID, err)
	}

	// Save snapshot metadata (absolute paths) so clones can reconstruct
	// storage/boot config without depending on live VM records.
	// FC snapshots require the same directory layout — paths are stored as-is.
	if metaErr := saveSnapshotMeta(tmpDir, rec.StorageConfigs, rec.BootConfig, rec.Config.CPU, rec.Config.Memory); metaErr != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck,gosec
		return nil, nil, fmt.Errorf("save snapshot metadata: %w", metaErr)
	}

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

	cfg := &types.SnapshotConfig{
		ID:            snapID,
		Image:         rec.Config.Image,
		Hypervisor:    typ,
		CPU:           rec.Config.CPU,
		Memory:        rec.Config.Memory,
		Storage:       rec.Config.Storage,
		NICs:          len(rec.NetworkConfigs),
		QueueSize:     rec.Config.QueueSize,
		DiskQueueSize: rec.Config.DiskQueueSize,
		Network:       rec.Config.Network,
	}
	if rec.ImageBlobIDs != nil {
		cfg.ImageBlobIDs = make(map[string]struct{}, len(rec.ImageBlobIDs))
		maps.Copy(cfg.ImageBlobIDs, rec.ImageBlobIDs)
	}

	return cfg, utils.TarDirStreamWithRemove(tmpDir), nil
}

// snapshotMeta is persisted as cocoon.json inside the snapshot tar.
// All paths are stored as absolute — FC snapshots require the same
// directory layout on the target host.
type snapshotMeta struct {
	StorageConfigs []*types.StorageConfig `json:"storage_configs"`
	BootConfig     *types.BootConfig      `json:"boot_config,omitempty"`
	CPU            int                    `json:"cpu,omitempty"`
	Memory         int64                  `json:"memory,omitempty"`
}

func saveSnapshotMeta(dir string, storageConfigs []*types.StorageConfig, boot *types.BootConfig, cpu int, memory int64) error {
	meta := snapshotMeta{CPU: cpu, Memory: memory}
	for _, sc := range storageConfigs {
		meta.StorageConfigs = append(meta.StorageConfigs, &types.StorageConfig{
			Path: sc.Path, RO: sc.RO, Serial: sc.Serial,
		})
	}
	if boot != nil {
		b := *boot
		// Store vmlinuz (portable), not vmlinux (FC-specific cache).
		if filepath.Base(b.KernelPath) == "vmlinux" {
			b.KernelPath = filepath.Join(filepath.Dir(b.KernelPath), "vmlinuz")
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

// loadSnapshotMeta reads cocoon.json and validates all paths are under
// Cocoon-managed directories. Rejects tampered archives with paths
// pointing outside rootDir/runDir.
func loadSnapshotMeta(dir, rootDir, runDir string) (*snapshotMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotMetaFile)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", snapshotMetaFile, err)
	}
	var meta snapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode %s: %w", snapshotMetaFile, err)
	}
	for _, sc := range meta.StorageConfigs {
		if !isUnderDir(sc.Path, rootDir) && !isUnderDir(sc.Path, runDir) {
			return nil, fmt.Errorf("untrusted storage path in snapshot metadata: %s", sc.Path)
		}
	}
	if b := meta.BootConfig; b != nil {
		if b.KernelPath != "" && !isUnderDir(b.KernelPath, rootDir) {
			return nil, fmt.Errorf("untrusted kernel path in snapshot metadata: %s", b.KernelPath)
		}
		if b.InitrdPath != "" && !isUnderDir(b.InitrdPath, rootDir) {
			return nil, fmt.Errorf("untrusted initrd path in snapshot metadata: %s", b.InitrdPath)
		}
	}
	return &meta, nil
}

func isUnderDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	cleaned := filepath.Clean(path)
	root := filepath.Clean(dir)
	return strings.HasPrefix(cleaned, root+string(filepath.Separator))
}
