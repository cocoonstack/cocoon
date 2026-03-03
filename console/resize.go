//go:build !windows

package console

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/term"
)

// HandleResize propagates the initial terminal size from localFd to remoteFd
// and listens for SIGWINCH to relay subsequent resize events.
// Returns a cleanup function that stops the signal handler.
func HandleResize(localFd, remoteFd uintptr) func() {
	syncSize := func() {
		if ws, err := term.GetWinsize(localFd); err == nil {
			_ = term.SetWinsize(remoteFd, ws)
		}
	}

	// Force SIGWINCH on initial connect: nudge the remote PTY to a
	// different size, then set the correct one. TIOCSWINSZ only sends
	// SIGWINCH when the dimensions actually change, so after vm.restore
	// the PTY may already have matching size and a plain SetWinsize
	// would be a no-op. The nudge guarantees a redraw.
	if ws, err := term.GetWinsize(localFd); err == nil {
		nudge := *ws
		nudge.Width++
		_ = term.SetWinsize(remoteFd, &nudge)
		_ = term.SetWinsize(remoteFd, ws)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for range sigCh {
			syncSize()
		}
	}()

	return func() {
		signal.Stop(sigCh)
		close(sigCh)
	}
}
