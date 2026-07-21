//go:build !linux

package daemon

import "errors"

var errNoProcNotify = errors.New("process-exit notification is unsupported on this OS")

// poller is inert off Linux: nothing is ever watched, so wait only parks until close.
type poller struct {
	done chan struct{}
}

func newPoller() (*poller, error) { return &poller{done: make(chan struct{})}, nil }

func (p *poller) add(int) error { return errNoProcNotify }

func (p *poller) remove(int) error { return nil }

func (p *poller) wait([]int) (int, error) {
	<-p.done
	return 0, errNoProcNotify
}

func (p *poller) close() error {
	close(p.done)
	return nil
}
