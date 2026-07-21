//go:build linux

package daemon

import (
	"errors"

	"golang.org/x/sys/unix"
)

// pollTimeoutMS bounds how long a wait blocks, so shutdown is not gated on a VM exiting.
const pollTimeoutMS = 500

// poller waits for pidfd readability, which the kernel signals once the process exits.
type poller struct {
	epfd int
}

func newPoller() (*poller, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &poller{epfd: epfd}, nil
}

func (p *poller) add(fd int) error {
	//nolint:gosec // a file descriptor never exceeds int32
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)})
}

func (p *poller) remove(fd int) error {
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (p *poller) wait(out []int) (int, error) {
	events := make([]unix.EpollEvent, len(out))
	n, err := unix.EpollWait(p.epfd, events, pollTimeoutMS)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, nil
		}
		return 0, err
	}
	for i := range n {
		out[i] = int(events[i].Fd)
	}
	return n, nil
}

func (p *poller) close() error { return unix.Close(p.epfd) }

func openPidfd(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

func closeFD(fd int) {
	if fd > 0 {
		_ = unix.Close(fd)
	}
}
