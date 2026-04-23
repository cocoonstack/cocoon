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

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// SnapshotFileKind classifies a snapshot file for CloneSnapshotFiles.
type SnapshotFileKind int

const (
	// SnapshotFileMemory is a read-only memory/state file (hard link or symlink).
	SnapshotFileMemory SnapshotFileKind = iota
	// SnapshotFileCOW is a writable disk that must be copied (reflink/sparse).
	SnapshotFileCOW
	// SnapshotFileMeta is small metadata that is plain-copied.
	SnapshotFileMeta
	// SnapshotFileSkip means the file should not be cloned.
	SnapshotFileSkip
)

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
			if s.RO {
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

// e.g., "/var/lib/cocoon/oci/blobs/abc123.erofs" → "abc123"
func BlobHexFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func PrefixToNetmask(prefix int) string {
	mask := net.CIDRMask(prefix, 32)
	return net.IP(mask).String()
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

// MergeDirInto renames entries from src to dst, overwriting existing files.
func MergeDirInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read staging dir: %w", err)
	}
	for _, e := range entries {
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

func InitCOWFilesystem(ctx context.Context, path string) error {
	// shell out because no Go ext4 formatter library; mkfs.ext4 is authoritative.
	out, err := exec.CommandContext(ctx, //nolint:gosec
		"mkfs.ext4", "-F", "-m", "0", "-q",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,discard",
		path,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func VerifyBaseFiles(storageConfigs []*types.StorageConfig, boot *types.BootConfig) error {
	for _, sc := range storageConfigs {
		if !sc.RO {
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

// CloneSnapshotFiles copies snapshot files using per-file strategies to minimize I/O.
func CloneSnapshotFiles(dstDir, srcDir string, classify func(name string) SnapshotFileKind) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read srcDir: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)

		switch classify(name) {
		case SnapshotFileMemory:
			// Hardlink for same-fs; symlink fallback for cross-fs (EXDEV only).
			// Hypervisors read memory files via MAP_PRIVATE, so neither
			// hardlink nor symlink will be modified.
			if linkErr := os.Link(src, dst); linkErr != nil {
				if !errors.Is(linkErr, syscall.EXDEV) {
					return fmt.Errorf("link %s: %w", name, linkErr)
				}
				if symlinkErr := os.Symlink(src, dst); symlinkErr != nil {
					return fmt.Errorf("symlink %s: %w", name, symlinkErr)
				}
			}
		case SnapshotFileCOW:
			if err := utils.ReflinkCopy(dst, src); err != nil {
				return fmt.Errorf("copy COW %s: %w", name, err)
			}
		case SnapshotFileMeta:
			if err := CopyFile(dst, src); err != nil {
				return fmt.Errorf("copy %s: %w", name, err)
			}
		case SnapshotFileSkip:
			// do nothing
		}
	}
	return nil
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
	return utils.WaitFor(ctx, timeout, 1*time.Millisecond, func() (bool, error) { //nolint:mnd
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
