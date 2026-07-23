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
	relayEnvKey           = "_COCOON_CONSOLE_RELAY"
	relayPIDEnvKey        = "_COCOON_FC_PID"
	relayLeaseCountEnvKey = "_COCOON_FC_LEASE_COUNT"
	relayBinaryEnvKey     = "_COCOON_FC_BINARY"
	relaySocketEnvKey     = "_COCOON_FC_SOCKET"

	// fd offsets for ExtraFiles (fd 3 = ExtraFiles[0]), in append order
	relayMasterFD        = 3
	relayListenerFD      = relayMasterFD + 1
	relayLeaseControlFD  = relayListenerFD + 1
	relayLeaseResponseFD = relayLeaseControlFD + 1
	relayLeaseFD         = relayLeaseResponseFD + 1

	relayPollInterval        = time.Second
	relayAbortRetryInterval  = time.Second
	relayAbortGrace          = time.Second
	relayBufSize             = 4096
	relayCommitLeasesCommand = byte(1)
	relayLeaseReadyCommand   = byte(2)
	relayLeaseCommitAck      = byte(3)
)

// broadcaster fans out PTY master reads to the single active console session, swapped atomically.
type broadcaster struct {
	master io.Reader
	mu     sync.Mutex
	sink   io.Writer // current session's conn; nil when no session
}

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

// RunRelay runs the console relay loop over inherited PTY, listener, and source-lease fds; one persistent PTY reader broadcasts so disconnects don't strand readers.
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

	leaseCount, validLeaseCount := relayInheritedLeaseCount()
	if !validLeaseCount {
		return
	}
	controlEnabled := leaseCount > 0
	leases := make([]*os.File, 0, leaseCount)
	for i := range leaseCount {
		leases = append(leases, os.NewFile(uintptr(relayLeaseFD+i), "source-vm-lease"))
	}
	var releaseLeasesOnce sync.Once
	releaseLeases := func() {
		releaseLeasesOnce.Do(func() {
			for _, lease := range leases {
				_ = lease.Close()
			}
		})
	}
	defer releaseLeases()

	pidStr := os.Getenv(relayPIDEnvKey)
	fcPID, pidErr := strconv.Atoi(pidStr)
	if pidErr != nil || fcPID <= 0 {
		log.WithFunc("firecracker.RunRelay").Warnf(ctx, "invalid FC PID %q, skipping relay", pidStr)
		return
	}

	ctx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	stop := func() {
		stopRelay()
		_ = listener.Close()
	}

	binaryName := os.Getenv(relayBinaryEnvKey)
	sockPath := os.Getenv(relaySocketEnvKey)
	if controlEnabled {
		if binaryName == "" || sockPath == "" {
			return
		}
		control := os.NewFile(relayLeaseControlFD, "source-lease-control")
		responses := os.NewFile(relayLeaseResponseFD, "source-lease-responses")
		if _, err := responses.Write([]byte{relayLeaseReadyCommand}); err != nil {
			_ = responses.Close()
			_ = control.Close()
			abortRelayFirecracker(ctx, fcPID, binaryName, sockPath)
			return
		}
		go func() {
			defer control.Close()   //nolint:errcheck
			defer responses.Close() //nolint:errcheck
			waitRelayLeaseControl(control, responses, releaseLeases, func() {
				abortRelayFirecracker(ctx, fcPID, binaryName, sockPath)
				stop()
			})
		}()
	}

	// Monitor FC process — close listener when FC exits.
	go func() {
		for {
			if !utils.IsProcessAlive(fcPID) {
				stop()
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

func waitRelayLeaseControl(control io.Reader, responses io.Writer, release, abort func()) {
	var command [1]byte
	if _, err := io.ReadFull(control, command[:]); err == nil && command[0] == relayCommitLeasesCommand {
		release()
		// Best-effort ACK: the parent treats the commit as done either way — the relay must not kill a committed VM.
		_, _ = responses.Write([]byte{relayLeaseCommitAck})
		return
	}
	abort()
}

func abortRelayFirecracker(ctx context.Context, pid int, binaryName, sockPath string) {
	logger := log.WithFunc("firecracker.abortRelayFirecracker")
	for relayFirecrackerAlive(pid, binaryName, sockPath) {
		if err := utils.TerminateProcess(ctx, pid, binaryName, sockPath, relayAbortGrace); err != nil {
			logger.Warnf(ctx, "terminate Firecracker %d after clone parent exit: %v", pid, err)
		}
		if !relayFirecrackerAlive(pid, binaryName, sockPath) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(relayAbortRetryInterval):
		}
	}
}

func relayFirecrackerAlive(pid int, binaryName, sockPath string) bool {
	if binaryName != "" && sockPath != "" {
		return utils.VerifyProcessCmdline(pid, binaryName, sockPath)
	}
	return utils.IsProcessAlive(pid)
}

func relayInheritedLeaseCount() (int, bool) {
	count, err := strconv.Atoi(os.Getenv(relayLeaseCountEnvKey))
	if err != nil || count < 0 {
		return 0, false
	}
	return count, true
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
