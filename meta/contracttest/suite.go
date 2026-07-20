// Package contracttest is the engine-agnostic meta contract suite: every engine must pass it unmodified.
package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
)

const (
	nsAlpha = "alpha"
	nsBeta  = "beta"

	nameOrig = "orig"
)

var errForcedRollback = errors.New("forced rollback")

// Factory returns a fresh Store serving the given namespaces; the suite owns its lifetime.
type Factory func(t *testing.T, namespaces []string) meta.Store

// Run executes the full contract suite against factory.
func Run(t *testing.T, factory Factory) {
	t.Run("CRUD", func(t *testing.T) { testCRUD(t, factory) })
	t.Run("DetachedValues", func(t *testing.T) { testDetached(t, factory) })
	t.Run("Scope", func(t *testing.T) { testScope(t, factory) })
	t.Run("DurabilityContract", func(t *testing.T) { testDurability(t, factory) })
	t.Run("ForcedRetry", func(t *testing.T) { testForcedRetry(t, factory) })
	t.Run("CtxVsHeldWriter", func(t *testing.T) { testCtxVsHeldWriter(t, factory) })
	t.Run("ViewIsolation", func(t *testing.T) { testViewIsolation(t, factory) })
	t.Run("DeadlockFreedom", func(t *testing.T) { testDeadlockFreedom(t, factory) })
	t.Run("Events", func(t *testing.T) { testEvents(t, factory) })
}

// ForcedRetry wraps s so every Update closure runs twice — once rolled back, once for real — enforcing pure retryable closures.
func ForcedRetry(s meta.Store) meta.Store { return &retryStore{Store: s} }

type record struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func get1(t *testing.T, s meta.Store, c *meta.Collection[record]) (*record, error) {
	t.Helper()
	var rec *record
	err := s.View(t.Context(), []string{nsAlpha}, func(r meta.Reader) error {
		var err error
		rec, err = c.Get(t.Context(), r, "a")
		return err
	})
	return rec, err
}

func testCRUD(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")

	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &record{Name: "one", N: 1})
	})
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &record{Name: "dup", N: 9})
	}); !errors.Is(err, meta.ErrConflict) {
		t.Fatalf("duplicate insert: got %v, want ErrConflict", err)
	}
	got, err := get1(t, s, c)
	if err != nil || got.N != 1 {
		t.Fatalf("get after insert: %+v, %v", got, err)
	}

	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Replace(ctx, w, "a", &record{Name: "one", N: 2})
	})
	if got, _ = get1(t, s, c); got.N != 2 {
		t.Fatalf("get after replace: %+v", got)
	}
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Replace(ctx, w, "absent", &record{})
	}); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("replace absent: got %v, want ErrNotFound", err)
	}

	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Delete(ctx, w, "a") })
	if _, err := get1(t, s, c); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	// Absent delete is idempotent success, never ErrNotFound.
	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Delete(ctx, w, "a") })
}

func testDetached(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")

	in := &record{Name: nameOrig, N: 1}
	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Insert(ctx, w, "a", in) })
	in.Name = "mutated-after-insert"

	got, err := get1(t, s, c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != nameOrig {
		t.Fatalf("insert captured caller mutation: %q", got.Name)
	}
	got.Name = "mutated-after-get"
	if again, _ := get1(t, s, c); again.Name != nameOrig {
		t.Fatalf("get returned attached value: %q", again.Name)
	}

	if err := s.View(ctx, []string{nsAlpha}, func(r meta.Reader) error {
		return c.Scan(ctx, r, func(_ string, rec *record) error {
			rec.Name = "mutated-in-scan"
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if again, _ := get1(t, s, c); again.Name != nameOrig {
		t.Fatalf("scan exposed engine state: %q", again.Name)
	}

	// Raw SPI detachment: mutating returned bytes inside a committing Update must not reach engine state (aliasing would bypass PutRaw's checks).
	update(t, s, nsAlpha, func(w meta.Writer) error {
		raw, ok, err := w.GetRaw(ctx, nsAlpha, "records", "a")
		if err != nil || !ok {
			t.Fatalf("GetRaw: %v %v", ok, err)
		}
		for i := range raw {
			raw[i] = 'X'
		}
		return w.ScanRaw(ctx, nsAlpha, "records", func(_ string, raw json.RawMessage) error {
			for i := range raw {
				raw[i] = 'Y'
			}
			return nil
		})
	})
	if again, err := get1(t, s, c); err != nil || again.Name != nameOrig {
		t.Fatalf("raw read aliased engine state: %v %v", again, err)
	}
}

func testScope(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha, nsBeta})
	alpha := meta.NewCollection[record](s, nsAlpha, "records")
	beta := meta.NewCollection[record](s, nsBeta, "records")

	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		_, err := beta.Get(ctx, w, "x")
		return err
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("undeclared read: got %v, want ErrScope", err)
	}
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha, Read: []string{nsBeta}}, meta.CommitDurable, func(w meta.Writer) error {
		return beta.Insert(ctx, w, "x", &record{})
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("write to read namespace: got %v, want ErrScope", err)
	}
	if err := s.View(ctx, []string{nsAlpha}, func(r meta.Reader) error {
		_, err := beta.Get(ctx, r, "x")
		return err
	}); !errors.Is(err, meta.ErrScope) {
		t.Fatalf("view undeclared: got %v, want ErrScope", err)
	}
	// Declared read works while writing another namespace.
	update(t, s, nsBeta, func(w meta.Writer) error { return beta.Insert(ctx, w, "x", &record{N: 7}) })
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha, Read: []string{nsBeta}}, meta.CommitDurable, func(w meta.Writer) error {
		got, err := beta.Get(ctx, w, "x")
		if err != nil {
			return err
		}
		return alpha.Insert(ctx, w, "copy", got)
	}); err != nil {
		t.Fatal(err)
	}
}

func testDurability(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")

	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitRelaxed, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &record{})
	}); !errors.Is(err, meta.ErrDurabilityContract) {
		t.Fatalf("durable-default write in relaxed txn: got %v, want ErrDurabilityContract", err)
	}
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitRelaxed, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &record{N: 1}, meta.RelaxedOK)
	}); err != nil {
		t.Fatalf("relaxed opt-in write: %v", err)
	}
	if got, err := get1(t, s, c); err != nil || got.N != 1 {
		t.Fatalf("relaxed write not visible: %+v, %v", got, err)
	}
}

func testForcedRetry(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := ForcedRetry(factory(t, []string{nsAlpha}))
	c := meta.NewCollection[record](s, nsAlpha, "records")

	// The correct pattern: accumulate inside, publish only after nil return.
	var out []string
	runs := 0
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		runs++
		staged := []string{}
		for _, id := range []string{"a", "b"} {
			if err := c.Insert(ctx, w, id, &record{Name: id}); err != nil {
				return err
			}
			staged = append(staged, id)
		}
		out = staged
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if runs < 2 {
		t.Fatalf("forced-retry wrapper ran closure %d times, want >= 2", runs)
	}
	if len(out) != 2 {
		t.Fatalf("published result: %v", out)
	}
	// Effects committed exactly once despite the double run.
	if err := s.View(ctx, []string{nsAlpha}, func(r meta.Reader) error {
		recs, err := c.List(ctx, r)
		if err != nil {
			return err
		}
		if len(recs) != 2 {
			return fmt.Errorf("committed %d records, want 2", len(recs))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testCtxVsHeldWriter(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")

	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan struct{})
	var heldOnce sync.Once
	go func() {
		defer close(holderDone)
		_ = s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
			heldOnce.Do(func() { close(held) })
			<-release
			return c.Insert(ctx, w, "holder", &record{})
		})
	}()
	<-held
	defer func() { <-holderDone }()
	defer close(release)

	dctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Update(dctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "blocked", &record{})
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked writer: got %v, want deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx overrun: blocked %v past a 100ms deadline", elapsed)
	}
}

func testViewIsolation(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")
	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Insert(ctx, w, "a", &record{N: 1}) })

	attempted := make(chan struct{})
	done := make(chan struct{})
	if err := s.View(ctx, []string{nsAlpha}, func(r meta.Reader) error {
		first, err := c.Get(ctx, r, "a")
		if err != nil {
			return err
		}
		go func() {
			defer close(done)
			close(attempted)
			update(t, s, nsAlpha, func(w meta.Writer) error {
				return c.Replace(ctx, w, "a", &record{N: 2})
			})
		}()
		<-attempted
		time.Sleep(50 * time.Millisecond)
		second, err := c.Get(ctx, r, "a")
		if err != nil {
			return err
		}
		if first.N != second.N {
			return fmt.Errorf("view saw concurrent write: %d then %d", first.N, second.N)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	if got, _ := get1(t, s, c); got.N != 2 {
		t.Fatalf("post-view state: %+v", got)
	}
}

func testDeadlockFreedom(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha, nsBeta})
	alpha := meta.NewCollection[record](s, nsAlpha, "records")
	beta := meta.NewCollection[record](s, nsBeta, "records")
	update(t, s, nsAlpha, func(w meta.Writer) error { return alpha.Insert(ctx, w, "x", &record{}) })
	update(t, s, nsBeta, func(w meta.Writer) error { return beta.Insert(ctx, w, "x", &record{}) })

	run := func(write, read string, wc, rc *meta.Collection[record]) error {
		for range 50 {
			if err := s.Update(ctx, meta.Scope{Write: write, Read: []string{read}}, meta.CommitDurable, func(w meta.Writer) error {
				if _, err := rc.Get(ctx, w, "x"); err != nil {
					return err
				}
				return wc.Replace(ctx, w, "x", &record{})
			}); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make(chan error, 2)
	go func() { errs <- run(nsAlpha, nsBeta, alpha, beta) }()
	go func() { errs <- run(nsBeta, nsAlpha, beta, alpha) }()
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("mutually-inverse transactions deadlocked")
		}
	}
}

func testEvents(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection[record](s, nsAlpha, "records")

	ch, release, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Insert(ctx, w, "a", &record{}) })
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no event after same-process commit")
	}
}

type retryStore struct {
	meta.Store
}

func (r *retryStore) Update(ctx context.Context, sc meta.Scope, mode meta.CommitMode, fn func(meta.Writer) error) error {
	err := r.Store.Update(ctx, sc, mode, func(w meta.Writer) error {
		if err := fn(w); err != nil {
			return err
		}
		return errForcedRollback
	})
	if err != nil && !errors.Is(err, errForcedRollback) {
		return err
	}
	return r.Store.Update(ctx, sc, mode, fn)
}

func update(t *testing.T, s meta.Store, ns string, fn func(meta.Writer) error) {
	t.Helper()
	if err := s.Update(t.Context(), meta.Scope{Write: ns}, meta.CommitDurable, fn); err != nil {
		t.Fatal(err)
	}
}
