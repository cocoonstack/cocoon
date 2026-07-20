// Package meta is the unified metadata layer: record-granularity
// transactions over per-deployment engines (json today, sqlite planned).
// See docs/design/meta-store.md.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

const (
	// CommitDurable fsyncs on commit; the default durability class.
	CommitDurable CommitMode = iota
	// CommitRelaxed may lose the un-checkpointed tail on power loss; the store stays consistent.
	CommitRelaxed

	// RelaxedOK marks one write as acceptable to lose, permitting it inside a CommitRelaxed transaction.
	RelaxedOK WriteOpt = 1
)

var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("record conflict")
	ErrBusy     = errors.New("store busy")
	ErrCorrupt  = errors.New("store corrupt")
	ErrNoSpace  = errors.New("no space left on store")
	ErrIO       = errors.New("store io error")
	// ErrDurabilityContract reports a durable-default write inside a CommitRelaxed transaction (clause 5).
	ErrDurabilityContract = errors.New("durable-default write in relaxed transaction")
	// ErrScope reports access to a namespace the transaction did not declare (clause 2).
	ErrScope = errors.New("namespace not in transaction scope")
)

// CommitMode selects a transaction's durability class.
type CommitMode uint8

// WriteOpt modifies a single write; see RelaxedOK.
type WriteOpt uint8

// Seq numbers committed log entries: unique and strictly increasing per
// namespace over committed entries; numbers from rolled-back appends may be
// reused or burned.
type Seq uint64

// Scope declares, before the closure runs, every namespace a transaction
// touches: Write is the single namespace it may modify, Read the others it
// may read. Engines acquire all of them in one fixed global order before
// invoking the closure.
type Scope struct {
	Write string
	Read  []string
}

// Store is the engine-neutral transaction boundary. Closures must be pure
// and retryable: they may run more than once, all effects go through the
// handle, and results are published only after the transaction returns nil.
type Store interface {
	// View runs fn over a consistent snapshot of the given namespaces.
	View(ctx context.Context, nss []string, fn func(Reader) error) error
	// Update runs fn in a serializable transaction writing sc.Write; commit follows mode.
	Update(ctx context.Context, sc Scope, mode CommitMode, fn func(Writer) error) error
	// Events returns a coalesced change signal; release unsubscribes.
	Events(ctx context.Context) (<-chan struct{}, func(), error)
	Close() error
}

// Reader is the raw read SPI transactions hand to Collection/Log; values
// returned are detached from engine state.
type Reader interface {
	GetRaw(ctx context.Context, ns, table, id string) (json.RawMessage, bool, error)
	// ScanRaw yields records in the engine's stable order (json: insertion).
	ScanRaw(ctx context.Context, ns, table string, fn func(id string, raw json.RawMessage) error) error
}

// Writer is the raw write SPI; relaxedOK mirrors the per-op RelaxedOK opt-in.
type Writer interface {
	Reader
	PutRaw(ctx context.Context, ns, table, id string, raw json.RawMessage, relaxedOK bool) error
	DeleteRaw(ctx context.Context, ns, table, id string, relaxedOK bool) error
	NextSeq(ctx context.Context, ns, table string) (Seq, error)
	Mode() CommitMode
}

func relaxedOK(opts []WriteOpt) bool {
	return slices.Contains(opts, RelaxedOK)
}
