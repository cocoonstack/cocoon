package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/utils"
)

const (
	relayEnvKey    = "_COCOON_CONSOLE_RELAY"
	relayPIDEnvKey = "_COCOON_FC_PID"

	// fd offsets for ExtraFiles (fd 3 = ExtraFiles[0], fd 4 = ExtraFiles[1])
	relayMasterFD   = 3
	relayListenerFD = 4

	relayPollInterval = time.Second
	relayBufSize      = 4096
)

// broadcaster fans out PTY master reads to the active console session.
// Only one session is active at a time; the subscriber is swapped atomically.
type broadcaster struct {
	master io.Reader
	mu     sync.Mutex
	sink   io.Writer // current session's conn; nil when no session
}

// setSink sets (or clears) the active session writer.
func (b *broadcaster) setSink(w io.Writer) {
	b.mu.Lock()
	b.sink = w
	b.mu.Unlock()
}

// readLoop runs as the relay's single lifetime PTY-reader goroutine, writing to the current sink.
func (b *broadcaster) readLoop() {
	buf := make([]byte, relayBufSize)
	for {
		n, err := b.master.Read(buf)
		if n > 0 {
			b.mu.Lock()
			if b.sink != nil {
				_, _ = b.sink.Write(buf[:n])
			}
			b.mu.Unlock()
		}
		if err != nil {
			return // PTY closed (FC exited)
		}
	}
}

// IsRelayMode returns true when the process was started as a console relay.
func IsRelayMode() bool {
	return os.Getenv(relayEnvKey) == "1"
}

// RunRelay runs the console relay loop. Inherits fd 3 (PTY master), fd 4 (console.sock listener), $_COCOON_FC_PID.
// A single persistent goroutine reads the PTY and broadcasts to the active session so disconnects don't strand readers.
func RunRelay(ctx context.Context) {
	master := os.NewFile(relayMasterFD, "pty-master")
	defer master.Close() //nolint:errcheck

	listenerFile := os.NewFile(relayListenerFD, "console-listener")
	listener, err := net.FileListener(listenerFile)
	_ = listenerFile.Close()
	if err != nil {
		return
	}
	defer listener.Close() //nolint:errcheck

	pidStr := os.Getenv(relayPIDEnvKey)
	fcPID, pidErr := strconv.Atoi(pidStr)
	if pidErr != nil || fcPID <= 0 {
		log.WithFunc("firecracker.RunRelay").Warnf(ctx, "invalid FC PID %q, skipping relay", pidStr)
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Monitor FC process — close listener when FC exits.
	go func() {
		for {
			if !utils.IsProcessAlive(fcPID) {
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

	bc := &broadcaster{master: master}
	go bc.readLoop()

	// Accept loop — one active console session at a time.
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		relaySession(ctx, master, conn, bc)
	}
}

// relaySession handles one console connection: subscribes to the PTY broadcast, copies conn→master for input, and unsubscribes on disconnect.
func relaySession(ctx context.Context, master io.Writer, conn net.Conn, bc *broadcaster) {
	defer conn.Close() //nolint:errcheck

	bc.setSink(conn)
	defer bc.setSink(nil)

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(master, conn)
		close(done)
	}()

	select {
	case <-done: // client disconnected
	case <-ctx.Done(): // FC died
	}
	// Closing conn unblocks the io.Copy(master, conn) goroutine.
}
