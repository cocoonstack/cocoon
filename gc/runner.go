package gc

import (
	"context"
)

// runner lets Orchestrator hold heterogeneous Module[S] values; callers use Module[S] + Register.
type runner interface {
	getName() string
	recover(ctx context.Context) []error
	readSnapshot(ctx context.Context) (any, error)
	resolveTargets(ctx context.Context, snap any, others map[string]any) []string
	collect(ctx context.Context, ids []string, snap any) error
}
