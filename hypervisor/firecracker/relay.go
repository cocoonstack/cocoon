package firecracker

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
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
	relayBufSize      = 4096
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
// A single persistent goroutine reads from the PTY master for the
// lifetime of the relay. Each console session receives output via a
// broadcast mechanism — no per-session goroutine reads the PTY, so
// disconnecting a session never leaves stale readers competing for data.
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

	fcPid, _ := strconv.Atoi(os.Getenv(relayPIDEnvKey))
	if fcPid <= 0 {
		return // invalid PID, nothing to relay
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Monitor FC process — close listener when FC exits.
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

	// Persistent PTY reader: a single goroutine reads the PTY master and
	// broadcasts to the current console session. This prevents stale
	// goroutines from stealing data after a session disconnects.
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

// readLoop reads from the PTY master forever and writes to the current sink.
// Runs as a single goroutine for the relay's lifetime.
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

// relaySession handles one console connection: subscribes to the PTY broadcast,
// copies conn→master for input, and unsubscribes on disconnect.
func relaySession(ctx context.Context, master io.Writer, conn net.Conn, bc *broadcaster) {
	defer conn.Close() //nolint:errcheck

	// Subscribe this session to receive PTY output.
	bc.setSink(conn)
	defer bc.setSink(nil)

	// Copy conn→master (console input) in a goroutine.
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
	// setSink(nil) in defer stops broadcast to this conn.
}
