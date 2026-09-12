package console

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/term"
)

// HandleResize syncs localFd → remoteFd terminal size and relays SIGWINCH; returned cleanup stops the signal handler.
func HandleResize(localFd, remoteFd uintptr) func() {
	syncSize := func() {
		if ws, err := term.GetWinsize(localFd); err == nil {
			_ = term.SetWinsize(remoteFd, ws)
		}
	}
	syncSize()

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
