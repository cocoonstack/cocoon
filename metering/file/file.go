package file

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder appends entries under sync.Mutex; O_APPEND gives cross-process atomicity.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
}

// New opens or creates the append-only ledger at path.
func New(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-configured metering path
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &Recorder{f: f}, nil
}

// Emit swallows write errors so callers never block.
func (r *Recorder) Emit(ctx context.Context, e metering.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := e.WriteTo(r.f); err != nil {
		log.WithFunc("metering.file.Recorder.Emit").Warnf(ctx, "emit entry: %v", err)
	}
}
