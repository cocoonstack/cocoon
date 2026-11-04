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
	relaySocketEnvKey     = "_COCOON_FC_SOCKET"

	// relayMasterFD is ExtraFiles[0]; the relay fds follow in append order.
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

// broadcaster swaps the PTY reader's sink atomically.
type broadcaster struct {
	master io.Reader
	mu     sync.Mutex
	sink   io.Writer
}

func (b *broadcaster) setSink(w io.Writer) {
	b.mu.Lock()
	b.sink = w
	b.mu.Unlock()
}

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
			return
		}
	}
}

// cloneLeaseControl is the parent's half of the relay lease protocol.
type cloneLeaseControl struct {
	commands  *os.File
	responses *os.File
}

func (c *cloneLeaseControl) close() {
	if c == nil {
		return
	}
	_ = c.commands.Close()
	_ = c.responses.Close()
}

func (c *cloneLeaseControl) commit() error {
	if c == nil {
		return nil
	}
	defer c.close()
	if _, err := c.commands.Write([]byte{relayCommitLeasesCommand}); err != nil {
		return err
	}
	return waitRelayLeaseResponse(c.responses, relayLeaseCommitAck, "commit")
}

// IsRelayMode returns true when the process was started as a console relay.
func IsRelayMode() bool {
	return os.Getenv(relayEnvKey) == "1"
}

// RunRelay runs the console relay loop over inherited PTY, listener, and source-lease fds.
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

	leaseCount, _ := strconv.Atoi(os.Getenv(relayLeaseCountEnvKey))
	leases := make([]*os.File, 0, leaseCount)
	for i := range leaseCount {
		leases = append(leases, os.NewFile(uintptr(relayLeaseFD+i), "source-vm-lease"))
	}
	releaseLeases := sync.OnceFunc(func() {
		for _, lease := range leases {
			_ = lease.Close()
		}
	})
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

	sockPath := os.Getenv(relaySocketEnvKey)
	if leaseCount > 0 {
		control := os.NewFile(relayLeaseControlFD, "source-lease-control")
		responses := os.NewFile(relayLeaseResponseFD, "source-lease-responses")
		if _, err := responses.Write([]byte{relayLeaseReadyCommand}); err != nil {
			_ = responses.Close()
			_ = control.Close()
			abortRelayFirecracker(ctx, fcPID, sockPath)
			return
		}
		go func() {
			defer control.Close()   //nolint:errcheck
			defer responses.Close() //nolint:errcheck
			waitRelayLeaseControl(control, responses, releaseLeases, func() {
				abortRelayFirecracker(ctx, fcPID, sockPath)
				stop()
			})
		}()
	}

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
		// Best-effort ACK: the parent treats the commit as done either way.
		_, _ = responses.Write([]byte{relayLeaseCommitAck})
		return
	}
	abort()
}

func abortRelayFirecracker(ctx context.Context, pid int, sockPath string) {
	logger := log.WithFunc("firecracker.abortRelayFirecracker")
	for utils.VerifyProcessCmdline(pid, sockPath) {
		if err := utils.TerminateProcess(ctx, pid, sockPath, relayAbortGrace); err != nil {
			logger.Warnf(ctx, "terminate Firecracker %d after clone parent exit: %v", pid, err)
		}
		if !utils.VerifyProcessCmdline(pid, sockPath) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(relayAbortRetryInterval):
		}
	}
}

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
	case <-done:
	case <-ctx.Done(): // FC died
	}
	// Closing conn unblocks the io.Copy(master, conn) goroutine.
}
