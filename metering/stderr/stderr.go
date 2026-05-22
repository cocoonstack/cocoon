package stderr

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder writes one JSON entry per line; dev/debug only.
type Recorder struct {
	mu  sync.Mutex
	out io.Writer
}

func New() *Recorder { return &Recorder{out: os.Stderr} }

func (r *Recorder) Emit(_ context.Context, e metering.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = e.WriteTo(r.out)
}
