package utils

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestForEach_AllSucceed(t *testing.T) {
	result := ForEach(t.Context(), []string{"a", "b", "c"}, func(_ context.Context, _ string) error {
		return nil
	})

	if len(result.Succeeded) != 3 {
		t.Errorf("succeeded: got %d, want 3", len(result.Succeeded))
	}
	if len(result.Errors) != 0 {
		t.Errorf("errors: got %d, want 0", len(result.Errors))
	}
	if result.Err() != nil {
		t.Errorf("Err(): got %v, want nil", result.Err())
	}
}

func TestForEach_AllFail(t *testing.T) {
	result := ForEach(t.Context(), []string{"x", "y"}, func(_ context.Context, id string) error {
		return fmt.Errorf("fail %s", id)
	})

	if len(result.Succeeded) != 0 {
		t.Errorf("succeeded: got %d, want 0", len(result.Succeeded))
	}
	if len(result.Errors) != 2 {
		t.Errorf("errors: got %d, want 2", len(result.Errors))
	}
	if result.Err() == nil {
		t.Error("Err(): expected non-nil")
	}
}

func TestForEach_PartialFailure(t *testing.T) {
	result := ForEach(t.Context(), []string{"ok", "fail", "ok2"}, func(_ context.Context, id string) error {
		if id == "fail" {
			return fmt.Errorf("error on %s", id)
		}
		return nil
	})

	if len(result.Succeeded) != 2 {
		t.Errorf("succeeded: got %v, want 2", result.Succeeded)
	}
	if len(result.Errors) != 1 {
		t.Errorf("errors: got %d, want 1", len(result.Errors))
	}
}

func TestForEach_EmptyIDs(t *testing.T) {
	result := ForEach(t.Context(), nil, func(_ context.Context, _ string) error {
		t.Fatal("should not be called")
		return nil
	})

	if len(result.Succeeded) != 0 || len(result.Errors) != 0 {
		t.Errorf("expected empty result, got %+v", result)
	}
	if result.Err() != nil {
		t.Errorf("Err(): got %v, want nil", result.Err())
	}
}

func TestBatchResult_Err_NilForNoErrors(t *testing.T) {
	r := BatchResult[string]{Succeeded: []string{"a"}}
	if r.Err() != nil {
		t.Errorf("expected nil, got %v", r.Err())
	}
}

func TestForEach_SingleID(t *testing.T) {
	result := ForEach(t.Context(), []string{"only"}, func(_ context.Context, id string) error {
		if id != "only" {
			t.Errorf("unexpected id: %s", id)
		}
		return nil
	})
	if len(result.Succeeded) != 1 {
		t.Errorf("succeeded: got %d, want 1", len(result.Succeeded))
	}
}

func TestForEach_Concurrent(t *testing.T) {
	var peak atomic.Int32
	var cur atomic.Int32

	result := ForEach(t.Context(), []string{"a", "b", "c", "d", "e"}, func(_ context.Context, _ string) error {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		cur.Add(-1)
		return nil
	})

	if len(result.Succeeded) != 5 {
		t.Errorf("succeeded: got %d, want 5", len(result.Succeeded))
	}
	if p := peak.Load(); p < 2 {
		t.Errorf("expected concurrent execution (peak=%d), got sequential", p)
	}
}

func TestForEach_WithConcurrencyLimit(t *testing.T) {
	var peak atomic.Int32
	var cur atomic.Int32

	result := ForEach(t.Context(), []string{"a", "b", "c", "d", "e"}, func(_ context.Context, _ string) error {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		cur.Add(-1)
		return nil
	}, 2)

	if len(result.Succeeded) != 5 {
		t.Errorf("succeeded: got %d, want 5", len(result.Succeeded))
	}
	if p := peak.Load(); p > 2 {
		t.Errorf("concurrency limit violated: peak=%d, limit=2", p)
	}
	if p := peak.Load(); p < 2 {
		t.Errorf("concurrency limit not reached: peak=%d, expected 2", p)
	}
}

func TestForEach_IntItems(t *testing.T) {
	result := ForEach(t.Context(), []int{1, 2, 3}, func(_ context.Context, n int) error {
		if n == 2 {
			return fmt.Errorf("bad number")
		}
		return nil
	})

	if len(result.Succeeded) != 2 {
		t.Errorf("succeeded: got %d, want 2", len(result.Succeeded))
	}
	if len(result.Errors) != 1 {
		t.Errorf("errors: got %d, want 1", len(result.Errors))
	}
}

func TestMap_AllSucceed(t *testing.T) {
	results, err := Map(t.Context(), []int{1, 2, 3}, func(_ context.Context, _, n int) (string, error) {
		return fmt.Sprintf("v%d", n), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"v1", "v2", "v3"}
	if len(results) != len(want) {
		t.Fatalf("results: got %d, want %d", len(results), len(want))
	}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d]: got %q, want %q", i, got, want[i])
		}
	}
}

func TestMap_FailFast(t *testing.T) {
	results, err := Map(t.Context(), []int{1, 2, 3}, func(_ context.Context, _, n int) (string, error) {
		if n == 2 {
			return "", fmt.Errorf("fail on %d", n)
		}
		return fmt.Sprintf("v%d", n), nil
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if results != nil {
		t.Errorf("expected nil results on error, got %v", results)
	}
}

func TestMap_Empty(t *testing.T) {
	results, err := Map(t.Context(), []int{}, func(_ context.Context, _, _ int) (string, error) {
		t.Fatal("should not be called")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil, got %v", results)
	}
}

func TestMap_PreservesOrder(t *testing.T) {
	results, err := Map(t.Context(), []int{10, 20, 30, 40, 50}, func(_ context.Context, _, n int) (int, error) {
		time.Sleep(time.Duration(50-n) * time.Millisecond) // reverse completion order
		return n * 2, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{20, 40, 60, 80, 100}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d]: got %d, want %d", i, got, want[i])
		}
	}
}

func TestMap_WithConcurrencyLimit(t *testing.T) {
	var peak atomic.Int32
	var cur atomic.Int32

	results, err := Map(t.Context(), []int{1, 2, 3, 4, 5}, func(_ context.Context, _, n int) (int, error) {
		c := cur.Add(1)
		for {
			old := peak.Load()
			if c <= old || peak.CompareAndSwap(old, c) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		cur.Add(-1)
		return n * 10, nil
	}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 5 {
		t.Errorf("results: got %d, want 5", len(results))
	}
	if p := peak.Load(); p > 2 {
		t.Errorf("concurrency limit violated: peak=%d, limit=2", p)
	}
}

func TestMap_IndexPassedCorrectly(t *testing.T) {
	items := []string{"a", "b", "c"}
	results, err := Map(t.Context(), items, func(_ context.Context, idx int, item string) (string, error) {
		return fmt.Sprintf("%d:%s", idx, item), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"0:a", "1:b", "2:c"}
	for i, got := range results {
		if got != want[i] {
			t.Errorf("results[%d]: got %q, want %q", i, got, want[i])
		}
	}
}
