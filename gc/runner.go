package gc

import (
	"context"

	"github.com/cocoonstack/cocoon/lock"
)

// runner lets Orchestrator hold heterogeneous Module[S] values; callers use Module[S] + Register.
type runner interface {
	getName() string
	getLocker() lock.Locker
	readSnapshot(ctx context.Context) (any, error)
	resolveTargets(ctx context.Context, snap any, others map[string]any) []string
	collect(ctx context.Context, ids []string, snap any) error
}
