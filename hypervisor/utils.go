package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/vishvananda/netns"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// SnapshotFileMemory is a read-only memory/state file (hard link or symlink).
	SnapshotFileMemory SnapshotFileKind = iota
	// SnapshotFileCOW is a writable disk that must be copied (reflink/sparse).
	SnapshotFileCOW
	// SnapshotFileMeta is small metadata that is plain-copied.
	SnapshotFileMeta
	// SnapshotFileSkip means the file should not be cloned.
	SnapshotFileSkip

	// OpsLockName is the per-VM cross-process mutation lock file (in the VM run dir).
	OpsLockName = "ops.lock"

	// restoreStagingName is the restore staging dir inside the VM run dir; snapshot capture dirs use the "snapshot-" prefix.
	restoreStagingName = ".restore-staging"
	captureDirPrefix   = "snapshot-"

	// MinDataDiskSize is the minimum user data disk size; mkfs.ext4 is unstable below this on small sparse files.
	MinDataDiskSize int64 = 16 << 20

	// socketReadyPollInterval is the WaitForSocket poll cadence — VMM socket usually appears within a few ms after process start.
	socketReadyPollInterval = 1 * time.Millisecond
)

// SnapshotFileKind classifies a snapshot file for CloneSnapshotFiles.
type SnapshotFileKind int

// LockVMOps serializes mutating verbs on one VM across processes (#103):
// device attach/detach, net resize, snapshot, hibernate, restore, stop.
// The flock dies with the process, so a crashed holder never wedges the VM.
func (b *Backend) LockVMOps(ctx context.Context, vmID string) (func(), error) {
	runDir := b.Conf.VMRunDir(vmID)
	// The record's persisted RunDir wins: after a --run-dir migration the paths differ and two lock files would let ops interleave.
	// Lockless read: RunDir is immutable after create, and a locked read would stall every ops verb behind an in-flight GC cycle's index lock. Fail closed on a real read error (ENOENT reads as empty) — guessing the path could split the lock domain.
	if err := b.DB.ReadRaw(func(idx *VMIndex) error {
		if r := idx.VMs[vmID]; r != nil && r.RunDir != "" {
			runDir = r.RunDir
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("resolve run dir for %s: %w", vmID, err)
	}
	l, err := opsLock(runDir)
	if err != nil {
		return nil, err
	}
	if err := l.Lock(ctx); err != nil {
		return nil, err
	}
	return func() { _ = l.Unlock(ctx) }, nil
}

func (b *Backend) PIDFilePath(runDir string) string {
	return filepath.Join(runDir, b.Conf.PIDFileName())
}

// LogFilePath returns the per-VM hypervisor log file under logDir (named after the backend so write/read can't drift).
func (b *Backend) LogFilePath(logDir string) string {
	return filepath.Join(logDir, b.Typ+".log")
}

// LogPath resolves ref → log path via the persisted LogDir (survives --log-dir change); falls back to current Conf for legacy records.
func (b *Backend) LogPath(ctx context.Context, ref string) (string, error) {
	id, err := b.ResolveRef(ctx, ref)
	if err != nil {
		return "", err
	}
	logDir := b.Conf.VMLogDir(id)
	if rec, err := b.LoadRecord(ctx, id); err == nil && rec.LogDir != "" {
		logDir = rec.LogDir
	}
	return b.LogFilePath(logDir), nil
}

// ForEachVM runs fn over ids in parallel up to EffectivePoolSize, logging per-id failures.
func (b *Backend) ForEachVM(ctx context.Context, ids []string, op string, fn func(context.Context, string) error) ([]string, error) {
	logger := log.WithFunc(b.Typ + "." + op)
	result := utils.ForEach(ctx, ids, fn, b.Conf.EffectivePoolSize())
	for _, err := range result.Errors {
		logger.Warnf(ctx, "%s: %v", op, err)
	}
	return result.Succeeded, result.Err()
}

// opsLock recreates runDir if missing (crash leftovers, logDir-only orphans) and returns the per-VM ops flock.
func opsLock(runDir string) (*flock.Lock, error) {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return nil, fmt.Errorf("ops lock dir: %w", err)
	}
	return flock.New(filepath.Join(runDir, OpsLockName)), nil
}

func SocketPath(runDir string) string { return filepath.Join(runDir, APISocketName) }

func ConsoleSockPath(runDir string) string { return filepath.Join(runDir, ConsoleSockName) }

func VsockSockPath(runDir string) string { return filepath.Join(runDir, VsockSockName) }

// BalloonSize returns (bytes, enabled); disabled on Windows (virtio-win driver loops on deflation) and below MinBalloonMemory.
func BalloonSize(memoryBytes int64, windows bool) (int64, bool) {
	if windows || memoryBytes < MinBalloonMemory {
		return 0, false
	}
	return memoryBytes / DefaultBalloonDiv, true
}

// IsDirectBoot reports whether boot uses a direct kernel (OCI) rather than UEFI firmware (cloudimg).
func IsDirectBoot(boot *types.BootConfig) bool {
	return boot != nil && boot.KernelPath != ""
}

func RemoveVMDirs(runDir, logDir string) error {
	return errors.Join(
		os.RemoveAll(runDir),
		os.RemoveAll(logDir),
	)
}

func CleanupRuntimeFiles(ctx context.Context, runDir string, files []string) {
	for _, name := range files {
		p := filepath.Join(runDir, name)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.WithFunc("hypervisor.CleanupRuntimeFiles").Warnf(ctx, "cleanup %s: %v", p, err)
		}
	}
}

func ExtractBlobIDs(storageConfigs []*types.StorageConfig, boot *types.BootConfig) map[string]struct{} {
	ids := make(map[string]struct{})
	if boot != nil && boot.KernelPath != "" {
		for _, s := range storageConfigs {
			if s.Role == types.StorageRoleLayer {
				ids[BlobHexFromPath(s.Path)] = struct{}{}
			}
		}
		ids[filepath.Base(filepath.Dir(boot.KernelPath))] = struct{}{}
		if boot.InitrdPath != "" {
			ids[filepath.Base(filepath.Dir(boot.InitrdPath))] = struct{}{}
		}
	} else if len(storageConfigs) > 0 {
		// Cloudimg: base qcow2 blob hex (before overlay replaces it).
		ids[BlobHexFromPath(storageConfigs[0].Path)] = struct{}{}
	}
	return ids
}

// BlobHexFromPath returns the digest hex of a blob path (e.g. .../abc123.erofs → abc123).
func BlobHexFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func PrefixToNetmask(prefix int) string {
	mask := net.CIDRMask(prefix, 32)
	return net.IP(mask).String()
}

// BuildBaseCmdline composes the cocoon-shared cmdline: prefix (backend-specific console+quirks) + boot/layers/cow + per-NIC ip= params.
func BuildBaseCmdline(prefix, layers, cow string, networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s boot=cocoon-overlay cocoon.layers=%s cocoon.cow=%s clocksource=kvm-clock rw", prefix, layers, cow)
	if len(networkConfigs) > 0 {
		b.WriteString(" net.ifnames=0")
		b.WriteString(BuildIPParams(networkConfigs, vmName, dnsServers))
	}
	return b.String()
}

func BuildIPParams(networkConfigs []*types.NetworkConfig, vmName string, dnsServers []string) string {
	var params strings.Builder
	fmt.Fprintf(&params, " cocoon.hostname=%s", vmName)
	var dns0, dns1 string
	if len(dnsServers) > 0 {
		dns0 = dnsServers[0]
	}
	if len(dnsServers) > 1 {
		dns1 = dnsServers[1]
	}
	for i, n := range networkConfigs {
		if n.Network == nil || n.Network.IP == "" {
			continue
		}
		param := fmt.Sprintf(" ip=%s::%s:%s:%s:eth%d:off",
			n.Network.IP, n.Network.Gateway,
			PrefixToNetmask(n.Network.Prefix), vmName, i)
		if dns0 != "" {
			param += ":" + dns0
			if dns1 != "" {
				param += ":" + dns1
			}
		}
		params.WriteString(param)
	}
	return params.String()
}

func CopyFile(dst, src string) (err error) {
	srcFile, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer srcFile.Close() //nolint:errcheck

	fi, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode()) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dstFile.Close()) }()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// MergeDirInto renames entries from src to dst, overwriting existing files; lock files never move (see isLockFile).
func MergeDirInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read staging dir: %w", err)
	}
	for _, e := range entries {
		if isLockFile(e.Name()) {
			continue
		}
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("rename %s to %s: %w", srcPath, dstPath, err)
		}
	}
	return nil
}

// isLockFile guards merge/extract/clone against snapshot payloads that would overwrite a held flock's inode — the next locker would then lock a fresh inode and mutual exclusion silently breaks.
func isLockFile(name string) bool {
	return name == OpsLockName || strings.HasSuffix(name, ".clone.lock")
}

func ValidateHostCPU(cpu int) error {
	maxCPU := runtime.NumCPU()
	if cpu > maxCPU {
		return fmt.Errorf("requested %d vCPUs exceeds host cores (%d)", cpu, maxCPU)
	}
	return nil
}

func InitCOWFilesystem(ctx context.Context, path string) error {
	// shell out because no Go ext4 formatter library; mkfs.ext4 is authoritative.
	out, err := exec.CommandContext(
		ctx, //nolint:gosec
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

// CopyWritableDisks reflinks the COW disk and every Role==Data disk into dstDir concurrently: inside the snapshot pause window, wall time is the longest single copy instead of the sum.
func CopyWritableDisks(ctx context.Context, dstDir, cowPath string, configs []*types.StorageConfig) error {
	pairs := [][2]string{{filepath.Join(dstDir, filepath.Base(cowPath)), cowPath}}
	for _, sc := range configs {
		if sc.Role == types.StorageRoleData {
			pairs = append(pairs, [2]string{filepath.Join(dstDir, DataDiskBaseName(sc.Serial)), sc.Path})
		}
	}
	return copyPairs(ctx, pairs)
}

// copyPairs runs ReflinkCopy over {dst, src} pairs concurrently; small pair counts (COW + data disks) need no pool bound.
func copyPairs(ctx context.Context, pairs [][2]string) error {
	_, err := utils.Map(ctx, pairs, func(_ context.Context, _ int, p [2]string) (struct{}, error) {
		if err := utils.ReflinkCopy(p[0], p[1]); err != nil {
			return struct{}{}, fmt.Errorf("copy %s: %w", filepath.Base(p[1]), err)
		}
		return struct{}{}, nil
	})
	return err
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
		path := filepath.Join(baseDir, DataDiskBaseName(spec.Name))
		if err := createSparseFile(path, spec.Size); err != nil {
			return nil, fmt.Errorf("data disk %s: %w", spec.Name, err)
		}
		switch spec.FSType {
		case types.FSTypeExt4:
			if err := InitCOWFilesystem(ctx, path); err != nil {
				return nil, fmt.Errorf("data disk %s: mkfs: %w", spec.Name, err)
			}
		case types.FSTypeNone:
			// raw, user formats inside guest
		default:
			return nil, fmt.Errorf("data disk %s: fstype %q not supported", spec.Name, spec.FSType)
		}
		out = append(out, &types.StorageConfig{
			Path:       path,
			RO:         false,
			Serial:     spec.Name,
			Role:       types.StorageRoleData,
			MountPoint: spec.MountPoint,
			FSType:     spec.FSType,
			DirectIO:   spec.DirectIO,
		})
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

// ValidateSnapshotIntegrity is the backend-agnostic preflight: sidecar is structurally valid and every snapshot-resident disk (COW/Cidata/Data) is on disk. Layers are shared blobs; backends add their own (state.json, vmstate) checks.
func ValidateSnapshotIntegrity(srcDir string, sidecar []*types.StorageConfig) error {
	if err := types.ValidateStorageConfigs(sidecar); err != nil {
		return fmt.Errorf("sidecar invalid: %w", err)
	}
	for _, sc := range sidecar {
		fname := snapshotResidentBasename(sc)
		if fname == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(srcDir, fname)); err != nil {
			return fmt.Errorf("snapshot file %s missing: %w", fname, err)
		}
	}
	return nil
}

// ValidateRoleSequence checks sidecar is a role+serial prefix of rec (an imported sidecar is untrusted — a swapped serial must not survive preflight); rec may carry trailing cidata (cloudimg post-first-boot) — the only allowed extension.
func ValidateRoleSequence(sidecar, rec []*types.StorageConfig) error {
	if len(sidecar) > len(rec) {
		return fmt.Errorf("snapshot has %d disks, record only %d", len(sidecar), len(rec))
	}
	for i, sc := range sidecar {
		if rec[i].Role != sc.Role {
			return fmt.Errorf("disk[%d] role mismatch: snapshot=%s record=%s", i, sc.Role, rec[i].Role)
		}
		if rec[i].Serial != sc.Serial {
			return fmt.Errorf("disk[%d] serial mismatch: snapshot=%q record=%q", i, sc.Serial, rec[i].Serial)
		}
	}
	for i := len(sidecar); i < len(rec); i++ {
		if rec[i].Role != types.StorageRoleCidata {
			return fmt.Errorf("disk[%d] only present in record must be cidata, got %s", i, rec[i].Role)
		}
	}
	return nil
}

// ExpandRawImage truncates path up to targetSize; no-op if path already meets it.
func ExpandRawImage(path string, targetSize int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if targetSize <= fi.Size() {
		return nil
	}
	if err := os.Truncate(path, targetSize); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", path, targetSize, err)
	}
	return nil
}

func VerifyBaseFiles(storageConfigs []*types.StorageConfig, boot *types.BootConfig) error {
	for _, sc := range storageConfigs {
		if sc.Role != types.StorageRoleLayer {
			continue
		}
		if _, err := os.Stat(sc.Path); err != nil {
			return fmt.Errorf("base layer %s: %w", sc.Path, err)
		}
	}
	if boot == nil {
		return nil
	}
	for _, check := range []struct{ name, path string }{
		{"kernel", boot.KernelPath},
		{"initrd", boot.InitrdPath},
		{"firmware", boot.FirmwarePath},
	} {
		if check.path == "" {
			continue
		}
		if _, err := os.Stat(check.path); err != nil {
			return fmt.Errorf("%s %s: %w", check.name, check.path, err)
		}
	}
	return nil
}

// CloneSnapshotFiles copies snapshot files using per-file strategies to minimize I/O; COW-class copies (the bulk of the bytes) fan out concurrently, links and small meta stay serial.
func CloneSnapshotFiles(ctx context.Context, dstDir, srcDir string, classify func(name string) SnapshotFileKind) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read srcDir: %w", err)
	}
	var cowPairs [][2]string
	for _, entry := range entries {
		if !entry.Type().IsRegular() || isLockFile(entry.Name()) {
			continue
		}
		name := entry.Name()
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)

		switch classify(name) {
		case SnapshotFileMemory:
			// Hardlink (same-fs); symlink fallback on EXDEV. Hypervisors MAP_PRIVATE the file so neither link is mutated.
			if linkErr := os.Link(src, dst); linkErr != nil {
				if !errors.Is(linkErr, syscall.EXDEV) {
					return fmt.Errorf("link %s: %w", name, linkErr)
				}
				if symlinkErr := os.Symlink(src, dst); symlinkErr != nil {
					return fmt.Errorf("symlink %s: %w", name, symlinkErr)
				}
			}
		case SnapshotFileCOW:
			cowPairs = append(cowPairs, [2]string{dst, src})
		case SnapshotFileMeta:
			if err := CopyFile(dst, src); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
		case SnapshotFileSkip:
		}
	}
	return copyPairs(ctx, cowPairs)
}

// CleanSnapshotFiles removes snapshot-specific files from runDir.
func CleanSnapshotFiles(runDir string, match func(name string) bool) error {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		t := entry.Type()
		if !t.IsRegular() && t&os.ModeSymlink == 0 {
			continue
		}
		if match(entry.Name()) {
			if removeErr := os.Remove(filepath.Join(runDir, entry.Name())); removeErr != nil {
				return fmt.Errorf("remove %s: %w", entry.Name(), removeErr)
			}
		}
	}
	return nil
}

func WaitForSocket(ctx context.Context, socketPath string, pid int, timeout time.Duration, processName string) error {
	return utils.WaitFor(ctx, timeout, socketReadyPollInterval, func() (bool, error) {
		if utils.CheckSocket(socketPath) == nil {
			return true, nil
		}
		if !utils.IsProcessAlive(pid) {
			return false, fmt.Errorf("%s exited before socket was ready", processName)
		}
		return false, nil
	})
}

func EnterNetns(nsPath string) (restore func(), err error) {
	runtime.LockOSThread()

	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("get current netns: %w", err)
	}

	target, err := netns.GetFromPath(nsPath)
	if err != nil {
		_ = orig.Close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("open netns %s: %w", nsPath, err)
	}
	defer target.Close() //nolint:errcheck

	if err := netns.Set(target); err != nil {
		_ = orig.Close()
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("setns %s: %w", nsPath, err)
	}

	return func() {
		_ = netns.Set(orig)
		_ = orig.Close()
		runtime.UnlockOSThread()
	}, nil
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

// snapshotResidentBasename returns the basename for sidecar entries inside srcDir; "" for shared base layers (not in tar).
func snapshotResidentBasename(sc *types.StorageConfig) string {
	switch sc.Role {
	case types.StorageRoleData:
		return DataDiskBaseName(sc.Serial)
	case types.StorageRoleCOW, types.StorageRoleCidata:
		return filepath.Base(sc.Path)
	default:
		return ""
	}
}
