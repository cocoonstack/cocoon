package stderr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder writes one JSON entry per line to a writer (os.Stderr by default); for dev / debug only.
type Recorder struct {
	mu  sync.Mutex
	out io.Writer
}

func New() *Recorder { return &Recorder{out: os.Stderr} }

// Emit best-effort writes; marshal errors silently dropped (stderr is fire-and-forget).
func (r *Recorder) Emit(_ context.Context, e metering.Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.out, string(data)) //nolint:errcheck // fire-and-forget; stderr write failures are not actionable
}
