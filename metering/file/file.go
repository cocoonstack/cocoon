package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder appends JSON-encoded entries one per line under sync.Mutex; cross-process atomicity comes from O_APPEND.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
}

func New(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &Recorder{f: f}, nil
}

// Emit logs and swallows write errors to never block callers.
func (r *Recorder) Emit(ctx context.Context, e metering.Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		log.WithFunc("metering/file.Recorder.Emit").Warnf(ctx, "marshal entry: %v", err)
		return
	}
	data = append(data, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.f.Write(data); err != nil {
		log.WithFunc("metering/file.Recorder.Emit").Warnf(ctx, "write entry: %v", err)
	}
}
