// Package capture is a metering recorder for tests; production code should not import it. Entries and Reset are testing-only helpers exposed on top of the Recorder contract.
package capture

import (
	"context"
	"sync"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

type Recorder struct {
	mu      sync.Mutex
	entries []metering.Entry
}

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
	out := make([]metering.Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}
