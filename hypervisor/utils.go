package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
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

	// CloneLocksDirName holds FC clone locks under the backend run root, outside any VM dir — a clone must be able to lock a source whose dir is already gone.
	CloneLocksDirName = "clone-locks"

	// restoreStagingName is the restore staging dir inside the VM run dir; snapshot capture dirs use the "snapshot-" prefix. restoreDirtyName is the tombstone marking the destructive restore phase — cleared only by FinalizeRestore, never by GC.
	restoreStagingName = ".restore-staging"
	restoreDirtyName   = ".restore-dirty"
	captureDirPrefix   = "snapshot-"

	// MinDataDiskSize is the minimum user data disk size; mkfs.ext4 is unstable below this on small sparse files.
	MinDataDiskSize int64 = 16 << 20

	// socketReadyPollInterval is the WaitForSocket poll cadence — VMM socket usually appears within a few ms after process start.
	socketReadyPollInterval = 1 * time.Millisecond
)

// SnapshotFileKind classifies a snapshot file for CloneSnapshotFiles.
type SnapshotFileKind int

// LockVMOps serializes mutating verbs on one VM across processes (#103); the flock dies with the holder, so a crash never wedges the VM.
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

func ValidateHostCPU(cpu int) error {
	maxCPU := runtime.NumCPU()
	if cpu > maxCPU {
		return fmt.Errorf("requested %d vCPUs exceeds host cores (%d)", cpu, maxCPU)
	}
	return nil
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
		// A degenerate name would stat a directory and vacuously pass.
		if fname == "." || fname == ".." || fname == string(filepath.Separator) {
			return fmt.Errorf("invalid snapshot disk name %q", fname)
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
	return copyPairs(ctx, cowPairs, utils.Sync)
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

// opsLock recreates runDir if missing (crash leftovers, logDir-only orphans) and returns the per-VM ops flock.
func opsLock(runDir string) (*flock.Lock, error) {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return nil, fmt.Errorf("ops lock dir: %w", err)
	}
	return flock.New(filepath.Join(runDir, OpsLockName)), nil
}

// isLockFile guards merge/extract/clone against snapshot payloads that would overwrite a held flock's inode — the next locker would then lock a fresh inode and mutual exclusion silently breaks.
func isLockFile(name string) bool {
	return name == OpsLockName || strings.HasSuffix(name, ".clone.lock")
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
