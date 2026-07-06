// Package capture is a test-only metering recorder; Entries and Reset are testing helpers.
package capture

import (
	"context"
	"slices"
	"sync"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder collects emitted entries in memory for test assertions.
type Recorder struct {
	mu      sync.Mutex
	entries []metering.Entry
}

// New returns an empty in-memory Recorder.
func New() *Recorder { return &Recorder{} }

func (r *Recorder) Emit(_ context.Context, e metering.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

// Entries returns a snapshot copy; callers may mutate the returned slice.
func (r *Recorder) Entries() []metering.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.entries)
}

func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}
