package utils

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sync/errgroup"
)

// BatchResult holds the outcome of a best-effort batch operation.
type BatchResult[T any] struct {
	Succeeded []T
	Errors    []error
}

// Err returns the combined error from all failed operations.
func (r BatchResult[T]) Err() error { return errors.Join(r.Errors...) }

// ForEach runs fn concurrently on each item, best-effort.
func ForEach[T any](ctx context.Context, items []T, fn func(context.Context, T) error, concurrency ...int) BatchResult[T] {
	limit := pickLimit(concurrency)

	type result struct {
		item T
		err  error
	}

	results := make([]result, len(items))
	g, gctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}

	for i, item := range items {
		g.Go(func() error {
			results[i] = result{item: item, err: fn(gctx, item)}
			return nil // always nil so errgroup processes all items (best-effort)
		})
	}
	_ = g.Wait()

	var r BatchResult[T]
	for _, res := range results {
		if res.err != nil {
			r.Errors = append(r.Errors, fmt.Errorf("item %v: %w", res.item, res.err))
		} else {
			r.Succeeded = append(r.Succeeded, res.item)
		}
	}
	return r
}

// Map runs fn concurrently and returns results in input order; fail-fast via errgroup. concurrency[0] caps in-flight goroutines (0 = unlimited).
func Map[T, R any](ctx context.Context, items []T, fn func(ctx context.Context, idx int, item T) (R, error), concurrency ...int) ([]R, error) {
	if len(items) == 0 {
		return nil, nil
	}

	limit := pickLimit(concurrency)

	results := make([]R, len(items))
	g, gctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}

	for i, item := range items {
		g.Go(func() error {
			r, err := fn(gctx, i, item)
			if err != nil {
				return err
			}
			results[i] = r
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// PoolSizeOrDefault returns n, or runtime.NumCPU() when n <= 0.
func PoolSizeOrDefault(n int) int {
	return OrDefault(n, runtime.NumCPU())
}

// OrDefault returns n, or def when n <= 0.
func OrDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

func pickLimit(concurrency []int) int {
	if len(concurrency) > 0 {
		return concurrency[0]
	}
	return 0
}
