package stderr

import (
	"context"
	"encoding/json"
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
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	data = append(data, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.out.Write(data)
}
