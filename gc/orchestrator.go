package gc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
)

// Orchestrator runs GC across all registered modules.
type Orchestrator struct {
	modules []runner
}

// New returns an Orchestrator with no registered modules.
func New() *Orchestrator { return &Orchestrator{} }

// Run executes one GC cycle: recover tombstones by phase, then snapshot → resolve → collect; modules revalidate every destructive decision under their own entity locks (§5).
func (o *Orchestrator) Run(ctx context.Context) error {
	start := time.Now()
	logger := log.WithFunc("gc.Run")

	var errs []error
	for _, m := range o.modules {
		for _, err := range m.recover(ctx) {
			errs = append(errs, fmt.Errorf("gc recover %s: %w", m.getName(), err))
		}
	}

	snapshots := make(map[string]any, len(o.modules))
	for _, m := range o.modules {
		snap, err := m.readSnapshot(ctx)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("gc aborted: snapshot %s: %w", m.getName(), err))...)
		}
		snapshots[m.getName()] = snap
	}

	targets := make(map[string][]string)
	for _, m := range o.modules {
		if ids := m.resolveTargets(ctx, snapshots[m.getName()], snapshots); len(ids) > 0 {
			targets[m.getName()] = ids
		}
	}

	summary := make(map[string]int, len(o.modules))
	for _, m := range o.modules {
		ids := targets[m.getName()]
		// Collect runs even with no ids: modules sweep stale temp/capture leftovers there.
		if err := m.collect(ctx, ids, snapshots[m.getName()]); err != nil {
			errs = append(errs, fmt.Errorf("gc %s: %w", m.getName(), err))
		}
		if len(ids) > 0 {
			summary[m.getName()] = len(ids)
		}
	}
	logger.Infof(ctx, "completed: %s (failures: %d, duration: %s)",
		formatSummary(summary), len(errs), time.Since(start).Truncate(time.Millisecond))
	return errors.Join(errs...)
}

// Register is package-level because Go methods can't have type params.
func Register[S any](o *Orchestrator, m Module[S]) {
	o.modules = append(o.modules, m)
}

// formatSummary renders counts as `m1=N m2=M`, sorted.
func formatSummary(s map[string]int) string {
	if len(s) == 0 {
		return "nothing to collect"
	}
	keys := slices.Sorted(maps.Keys(s))
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%s=%d", k, s[k])
	}
	return sb.String()
}
