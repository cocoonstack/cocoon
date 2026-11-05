package meta

import (
	"errors"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestBroadcasterWatchErrorRunsCheck(t *testing.T) {
	errs := make(chan error)
	b := NewBroadcaster(&fsnotify.Watcher{Events: make(chan fsnotify.Event), Errors: errs})
	checked := make(chan struct{}, 1)
	go b.Run(func() {
		select {
		case checked <- struct{}{}:
		default:
		}
	})
	defer close(b.done)

	errs <- errors.New("forced overflow")
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("watch error did not run check")
	}
}
