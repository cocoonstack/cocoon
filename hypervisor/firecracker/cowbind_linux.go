//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// launchWithBinds runs launch on a throwaway locked thread inside a private mount namespace with each dst bind-mounted over its src, so FC resolves source-absolute drive paths without symlink swaps. The thread is never unlocked: it dies with the goroutine, taking the namespace along.
func launchWithBinds(binds [][2]string, launch func() (int, error)) (int, error) {
	type result struct {
		pid int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		// Go threads share CLONE_FS; unshare it or CLONE_NEWNS is refused.
		if err := unix.Unshare(unix.CLONE_FS | unix.CLONE_NEWNS); err != nil {
			ch <- result{0, fmt.Errorf("%w: unshare: %w", errBindSetup, err)}
			return
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			ch <- result{0, fmt.Errorf("%w: make mounts private: %w", errBindSetup, err)}
			return
		}
		for _, b := range binds {
			if err := unix.Mount(b[1], b[0], "", unix.MS_BIND, ""); err != nil {
				ch <- result{0, fmt.Errorf("%w: bind %s over %s: %w", errBindSetup, b[1], b[0], err)}
				return
			}
		}
		pid, err := launch()
		ch <- result{pid, err}
	}()
	r := <-ch
	return r.pid, r.err
}

// verifyDriveFDs confirms pid holds an open fd for every bind's clone-side inode: unlinking a mountpoint path from another namespace detaches the bind (mount_namespaces(7)), so a concurrent source restore/delete inside the bind→load window would silently hand FC the source's writable disk.
func verifyDriveFDs(pid int, binds [][2]string) error {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", fdDir, err)
	}
	open := make(map[[2]uint64]struct{}, len(entries))
	for _, e := range entries {
		var st unix.Stat_t
		if statErr := unix.Stat(filepath.Join(fdDir, e.Name()), &st); statErr != nil {
			continue
		}
		open[[2]uint64{st.Dev, st.Ino}] = struct{}{}
	}
	for _, b := range binds {
		var st unix.Stat_t
		if statErr := unix.Stat(b[1], &st); statErr != nil {
			return fmt.Errorf("stat %s: %w", b[1], statErr)
		}
		if _, ok := open[[2]uint64{st.Dev, st.Ino}]; !ok {
			return fmt.Errorf("%s is not held open by the VMM", b[1])
		}
	}
	return nil
}
