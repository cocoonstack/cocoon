package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

const (
	relayEnvKey    = "_COCOON_CONSOLE_RELAY"
	relayPIDEnvKey = "_COCOON_FC_PID"

	// fd offsets for ExtraFiles (fd 3 = ExtraFiles[0], fd 4 = ExtraFiles[1])
	relayMasterFD   = 3
	relayListenerFD = 4

	relayPollInterval = time.Second
)

// IsRelayMode returns true when the process was started as a console relay.
func IsRelayMode() bool {
	return os.Getenv(relayEnvKey) == "1"
}

// RunRelay runs the console relay loop. Called from main.go when
// IsRelayMode() returns true. The relay process inherits:
//   - fd 3: PTY master (bidirectional serial I/O with FC)
//   - fd 4: Unix listener file (console.sock)
//   - _COCOON_FC_PID: Firecracker PID to monitor
//
// The relay accepts one connection at a time on console.sock and
// copies bidirectionally between the connection and the PTY master.
// It auto-exits when the Firecracker process dies.
func RunRelay() {
	master := os.NewFile(relayMasterFD, "pty-master")
	defer master.Close() //nolint:errcheck

	listenerFile := os.NewFile(relayListenerFD, "console-listener")
	listener, err := net.FileListener(listenerFile)
	_ = listenerFile.Close()
	if err != nil {
		return
	}
	defer listener.Close() //nolint:errcheck

	fcPid, _ := strconv.Atoi(os.Getenv(relayPIDEnvKey))

	// Monitor FC process — close listener when FC exits.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if !utils.IsProcessAlive(fcPid) {
				cancel()
				_ = listener.Close()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(relayPollInterval):
			}
		}
	}()

	// Accept loop — one active console session at a time.
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		relayBidirectional(ctx, master, conn)
		_ = conn.Close()
	}
}

// relayBidirectional copies data in both directions between a and b.
// Returns when either direction hits EOF/error or ctx is canceled.
func relayBidirectional(ctx context.Context, a, b io.ReadWriter) {
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}
