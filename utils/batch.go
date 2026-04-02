package utils

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// BatchResult holds the outcome of a best-effort batch operation.
type BatchResult[T any] struct {
	Succeeded []T
	Errors    []error
}

// Err returns the combined error from all failed operations.
func (r BatchResult[T]) Err() error { return errors.Join(r.Errors...) }

// ForEach runs fn for each item concurrently, collecting successes and errors
// (best-effort). All items are attempted regardless of individual failures.
// An optional concurrency limit caps in-flight goroutines; zero or omitted
// means no limit.
func ForEach[T any](ctx context.Context, items []T, fn func(context.Context, T) error, concurrency ...int) BatchResult[T] {
	if len(items) == 0 {
		return BatchResult[T]{}
	}

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
			r.Errors = append(r.Errors, fmt.Errorf("%v: %w", res.item, res.err))
		} else {
			r.Succeeded = append(r.Succeeded, res.item)
		}
	}
	return r
}

// Map runs fn for each item concurrently, returning results in input order.
// Fail-fast: the errgroup context is canceled on the first error, so
// in-flight callbacks see a canceled ctx and new ones are not started.
// An optional concurrency limit caps in-flight goroutines; zero or omitted
// means no limit.
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

func pickLimit(concurrency []int) int {
	if len(concurrency) > 0 {
		return concurrency[0]
	}
	return 0
}
