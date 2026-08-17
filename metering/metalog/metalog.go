// Package metalog records metering entries in the meta store's log (§1 P3): one Relaxed append per entry (the file backend's no-fsync durability) with the Seq cursor committed alongside.
package metalog

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
)

const (
	// NamespaceName/TableEntries locate the metering log in the meta store.
	NamespaceName = "metering"
	TableEntries  = "entries"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder appends entries through the meta store; Emit swallows errors so callers never block.
type Recorder struct {
	store meta.Store
	log   *meta.Log[metering.Entry]
}

// New binds a Recorder to the process-wide meta store.
func New(s meta.Store) *Recorder {
	return &Recorder{store: s, log: meta.NewLog[metering.Entry](NamespaceName, TableEntries)}
}

func (r *Recorder) Emit(ctx context.Context, e metering.Entry) {
	err := r.store.Update(ctx, meta.Scope{Write: NamespaceName}, meta.CommitRelaxed, func(w meta.Writer) error {
		_, aerr := r.log.Append(ctx, w, &e, meta.RelaxedOK)
		return aerr
	})
	if err != nil {
		log.WithFunc("metering.metalog.Recorder.Emit").Warnf(ctx, "append entry: %v", err)
	}
}

// Scan streams committed entries with Seq strictly after cursor.
func (r *Recorder) Scan(ctx context.Context, after meta.Seq, fn func(meta.Seq, *metering.Entry) error) error {
	return r.store.View(ctx, []string{NamespaceName}, func(rd meta.Reader) error {
		return r.log.Scan(ctx, rd, after, fn)
	})
}
