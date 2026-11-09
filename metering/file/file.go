// Package file records metering entries in an append-only ledger file.
package file

import (
	"context"
	"fmt"
	"os"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/metering"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder appends one O_APPEND write per entry, atomic across processes; Emit logs and drops write errors.
type Recorder struct {
	f *os.File
}

// New opens or creates the append-only ledger at path.
func New(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-configured metering path
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &Recorder{f: f}, nil
}

func (r *Recorder) Emit(ctx context.Context, e metering.Entry) {
	if _, err := e.WriteTo(r.f); err != nil {
		log.WithFunc("metering.file.Emit").Error(ctx, err, "emit entry")
	}
}
