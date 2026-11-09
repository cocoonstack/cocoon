// Package metalog records metering entries in the meta store's log, one relaxed append per entry.
package metalog

import (
	"context"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/metering"
)

const (
	NamespaceName = "metering"
	TableEntries  = "entries"
)

var _ metering.Recorder = (*Recorder)(nil)

// Recorder appends entries through the meta store; Emit logs and drops append errors.
type Recorder struct {
	store meta.Store
	log   *meta.Log[metering.Entry]
}

// New binds a Recorder to the process-wide meta store.
func New(s meta.Store) *Recorder {
	return &Recorder{store: s, log: meta.NewLog[metering.Entry](NamespaceName, TableEntries)}
}

func (r *Recorder) Emit(ctx context.Context, e metering.Entry) {
	ctx = context.WithoutCancel(ctx)
	err := r.store.Update(ctx, meta.Scope{Write: NamespaceName}, meta.CommitRelaxed, func(w meta.Writer) error {
		_, aerr := r.log.Append(ctx, w, &e, meta.RelaxedOK)
		return aerr
	})
	if err != nil {
		log.WithFunc("metering.metalog.Emit").Error(ctx, err, "append entry")
	}
}
