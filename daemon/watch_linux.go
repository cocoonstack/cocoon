//go:build linux

package daemon

import (
	"errors"

	"golang.org/x/sys/unix"
)

const (
	// pollTimeoutMS bounds how long a wait blocks, so shutdown is not gated on a VM exiting.
	pollTimeoutMS = 500
	maxPollEvents = 64
)

// poller waits for pidfd readability, which the kernel signals once the process exits.
type poller struct {
	epfd   int
	events []unix.EpollEvent
}

func newPoller() (*poller, error) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &poller{epfd: epfd, events: make([]unix.EpollEvent, maxPollEvents)}, nil
}

func (p *poller) add(fd int) error {
	//nolint:gosec // a file descriptor never exceeds int32
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)})
}

func (p *poller) remove(fd int) error {
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (p *poller) wait(out []int) (int, error) {
	n, err := unix.EpollWait(p.epfd, p.events[:min(len(out), maxPollEvents)], pollTimeoutMS)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, nil
		}
		return 0, err
	}
	for i := range n {
		out[i] = int(p.events[i].Fd)
	}
	return n, nil
}

func (p *poller) close() error { return unix.Close(p.epfd) }
