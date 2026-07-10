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

// Register is package-level because Go methods can't have type params.
func Register[S any](o *Orchestrator, m Module[S]) {
	o.modules = append(o.modules, m)
}

// Run executes one GC cycle: lock all → snapshot → resolve → collect. Fail-closed on busy locks to keep cross-module decisions consistent.
func (o *Orchestrator) Run(ctx context.Context) error {
	start := time.Now()
	logger := log.WithFunc("gc.Run")

	var locked []runner
	var skipped []string
	for _, m := range o.modules {
		ok, err := m.getLocker().TryLock(ctx)
		if err != nil {
			logger.Warnf(ctx, "skip %s: TryLock error: %v", m.getName(), err)
			skipped = append(skipped, m.getName())
			continue
		}
		if !ok {
			logger.Warnf(ctx, "skip %s: lock held by another operation", m.getName())
			skipped = append(skipped, m.getName())
			continue
		}
		locked = append(locked, m)
	}
	defer func() {
		for _, m := range locked {
			m.getLocker().Unlock(ctx) //nolint:errcheck,gosec
		}
	}()

	if len(skipped) > 0 {
		return fmt.Errorf("gc aborted: modules skipped (lock busy): %s", strings.Join(skipped, ", "))
	}

	snapshots := make(map[string]any, len(locked))
	for _, m := range locked {
		snap, err := m.readSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("gc aborted: snapshot %s: %w", m.getName(), err)
		}
		snapshots[m.getName()] = snap
	}

	targets := make(map[string][]string)
	for _, m := range locked {
		if ids := m.resolveTargets(ctx, snapshots[m.getName()], snapshots); len(ids) > 0 {
			targets[m.getName()] = ids
		}
	}

	var errs []error
	summary := make(map[string]int, len(locked))
	for _, m := range locked {
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
