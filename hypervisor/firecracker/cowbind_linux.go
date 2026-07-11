//go:build linux

package firecracker

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// launchWithBinds runs launch on a throwaway locked thread inside a private mount namespace with each dst bind-mounted over its src, so FC resolves source-absolute drive paths without touching the host namespace. The thread is never unlocked: it dies with the goroutine, taking the namespace along.
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
