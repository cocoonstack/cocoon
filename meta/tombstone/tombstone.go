// Package tombstone is the §5 phase protocol: leased rolls back, deleting
// rolls forward, every mutation fenced by the holder's lease id.
package tombstone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	// TableName is the tombstones satellite table every §5 namespace carries.
	TableName = "tombstones"

	PhaseLeased   Phase = "leased"
	PhaseDeleting Phase = "deleting"

	KindRecord Kind = "record"
	KindOrphan Kind = "orphan"

	ModeAggregate Mode = "aggregate"
	ModeSubset    Mode = "subset"
)

// ErrLost reports a fenced write that matched zero rows: another worker
// recovered and finalized this lease after its owner died.
var ErrLost = errors.New("tombstone lease lost")

// Phase is the tombstone's protocol position.
type Phase string

// Kind distinguishes a record-backed candidate from a recordless orphan.
type Kind string

// Mode distinguishes a full teardown from an explicit subset; recovering a
// subset as an aggregate would destroy healthy resources.
type Mode string

// Payload is written whole at lease time and immutable after: recovery
// reads it and nothing else.
type Payload struct {
	Kind    Kind            `json:"kind"`
	Mode    Mode            `json:"mode"`
	Cleanup json.RawMessage `json:"cleanup"`
}

// Record is one tombstone row.
type Record struct {
	LeaseID  string    `json:"lease_id"`
	Phase    Phase     `json:"phase"`
	LeasedAt time.Time `json:"leased_at"`
	Payload  Payload   `json:"payload"`
}

// Table binds one namespace's tombstone satellite set.
type Table struct {
	ns   string
	recs *meta.Collection[Record]
}

// NewTable binds the tombstones table in ns on s.
func NewTable(s meta.Store, ns string) *Table {
	return &Table{ns: ns, recs: meta.NewCollection[Record](s, ns, TableName)}
}

// Get returns id's tombstone, nil when none.
func (t *Table) Get(ctx context.Context, r meta.Reader, id string) (*Record, error) {
	rec, err := t.recs.Get(ctx, r, id)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, nil
	}
	return rec, err
}

// Scan yields every tombstone (the recovery sweep runs before discovery).
func (t *Table) Scan(ctx context.Context, r meta.Reader, fn func(id string, rec *Record) error) error {
	return t.recs.Scan(ctx, r, fn)
}

// Lease inserts id's tombstone with a fresh lease and its complete payload;
// an existing tombstone is ErrConflict — another worker owns the candidate.
func (t *Table) Lease(ctx context.Context, w meta.Writer, id string, p Payload) (string, error) {
	leaseID := utils.GenerateID()
	if err := t.recs.Insert(ctx, w, id, &Record{LeaseID: leaseID, Phase: PhaseLeased, LeasedAt: time.Now(), Payload: p}); err != nil {
		return "", err
	}
	return leaseID, nil
}

// TakeOver replaces a dead owner's lease with a fresh one, preserving phase
// and payload; the caller must hold the entity lock.
func (t *Table) TakeOver(ctx context.Context, w meta.Writer, id string) (*Record, error) {
	rec, err := t.Get(ctx, w, id)
	if err != nil || rec == nil {
		return nil, err
	}
	rec.LeaseID = utils.GenerateID()
	rec.LeasedAt = time.Now()
	if err := t.recs.Replace(ctx, w, id, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// MarkDeleting flips the tombstone to the deleting phase, fenced by leaseID;
// it is committed BEFORE any filesystem work.
func (t *Table) MarkDeleting(ctx context.Context, w meta.Writer, id, leaseID string) error {
	rec, err := t.fenced(ctx, w, id, leaseID)
	if err != nil {
		return err
	}
	rec.Phase = PhaseDeleting
	return t.recs.Replace(ctx, w, id, rec)
}

// Rollback removes a still-leased tombstone (no filesystem work happened),
// fenced by leaseID.
func (t *Table) Rollback(ctx context.Context, w meta.Writer, id, leaseID string) error {
	rec, err := t.fenced(ctx, w, id, leaseID)
	if err != nil {
		return err
	}
	if rec.Phase != PhaseLeased {
		return fmt.Errorf("tombstone %s/%s is %s: %w", t.ns, id, rec.Phase, meta.ErrConflict)
	}
	return t.recs.Delete(ctx, w, id)
}

// Finalize removes the tombstone after cleanup, fenced by leaseID.
func (t *Table) Finalize(ctx context.Context, w meta.Writer, id, leaseID string) error {
	if _, err := t.fenced(ctx, w, id, leaseID); err != nil {
		return err
	}
	return t.recs.Delete(ctx, w, id)
}

// PendingIDs lists every tombstoned id (the recovery sweep's work list).
func (t *Table) PendingIDs(ctx context.Context, r meta.Reader) ([]string, error) {
	var ids []string
	if err := t.Scan(ctx, r, func(id string, _ *Record) error {
		ids = append(ids, id)
		return nil
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

// Acquire starts protocol work on id under the held entity lock: takes over
// an existing tombstone (resumed reports it), or leases fresh from build.
func (t *Table) Acquire(ctx context.Context, w meta.Writer, id string, build func() (Payload, error)) (leaseID string, resumed *Record, err error) {
	existing, err := t.Get(ctx, w, id)
	if err != nil {
		return "", nil, err
	}
	if existing != nil {
		taken, takeErr := t.TakeOver(ctx, w, id)
		if takeErr != nil {
			return "", nil, takeErr
		}
		return taken.LeaseID, taken, nil
	}
	p, err := build()
	if err != nil {
		return "", nil, err
	}
	leaseID, err = t.Lease(ctx, w, id, p)
	return leaseID, nil, err
}

// Resume takes over id's tombstone for recovery under the held entity lock:
// a leased entry rolls back in place; a deleting one gets a fresh lease for
// the caller to roll forward.
func (t *Table) Resume(ctx context.Context, w meta.Writer, id string) (rec *Record, leaseID string, err error) {
	rec, err = t.Get(ctx, w, id)
	if err != nil || rec == nil {
		return rec, "", err
	}
	taken, err := t.TakeOver(ctx, w, id)
	if err != nil {
		return nil, "", err
	}
	if rec.Phase == PhaseLeased {
		return rec, taken.LeaseID, t.Rollback(ctx, w, id, taken.LeaseID)
	}
	return rec, taken.LeaseID, nil
}

func (t *Table) fenced(ctx context.Context, w meta.Writer, id, leaseID string) (*Record, error) {
	rec, err := t.Get(ctx, w, id)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.LeaseID != leaseID {
		return nil, fmt.Errorf("tombstone %s/%s: %w", t.ns, id, ErrLost)
	}
	return rec, nil
}

// MarshalCleanup encodes a namespace-defined cleanup payload.
func MarshalCleanup(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode cleanup payload: %w", err)
	}
	return raw, nil
}
