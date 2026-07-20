// Package contracttest is the engine-agnostic meta contract suite: every
// engine must pass it unmodified (design §8, §9).
package contracttest

import (
	"context"
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

// Factory returns a fresh Store serving the given namespaces; the suite owns
// its lifetime.
type Factory func(t *testing.T, namespaces []string) meta.Store

// Run executes the full contract suite against factory.
func Run(t *testing.T, factory Factory) {
	t.Run("CRUD", func(t *testing.T) { testCRUD(t, factory) })
	t.Run("DetachedValues", func(t *testing.T) { testDetached(t, factory) })
	t.Run("UniqueIndex", func(t *testing.T) { testUniqueIndex(t, factory) })
	t.Run("Scope", func(t *testing.T) { testScope(t, factory) })
	t.Run("DurabilityContract", func(t *testing.T) { testDurability(t, factory) })
	t.Run("ForcedRetry", func(t *testing.T) { testForcedRetry(t, factory) })
	t.Run("LogSeq", func(t *testing.T) { testLogSeq(t, factory) })
	t.Run("CtxVsHeldWriter", func(t *testing.T) { testCtxVsHeldWriter(t, factory) })
	t.Run("ViewIsolation", func(t *testing.T) { testViewIsolation(t, factory) })
	t.Run("DeadlockFreedom", func(t *testing.T) { testDeadlockFreedom(t, factory) })
	t.Run("Events", func(t *testing.T) { testEvents(t, factory) })
}

// ForcedRetry wraps s so every Update closure runs twice — once rolled back,
// once for real — enforcing contract clause 1 (pure retryable closures).
func ForcedRetry(s meta.Store) meta.Store { return &retryStore{Store: s} }

type record struct {
	Name string `json:"name"`
	N    int    `json:"n"`
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
	got, err := c.Get1(ctx, "a")
	if err != nil || got.N != 1 {
		t.Fatalf("get after insert: %+v, %v", got, err)
	}

	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Replace(ctx, w, "a", &record{Name: "one", N: 2})
	})
	if got, _ = c.Get1(ctx, "a"); got.N != 2 {
		t.Fatalf("get after replace: %+v", got)
	}
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Replace(ctx, w, "absent", &record{})
	}); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("replace absent: got %v, want ErrNotFound", err)
	}

	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Delete(ctx, w, "a") })
	if _, err := c.Get1(ctx, "a"); !errors.Is(err, meta.ErrNotFound) {
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

	got, err := c.Get1(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != nameOrig {
		t.Fatalf("insert captured caller mutation: %q", got.Name)
	}
	got.Name = "mutated-after-get"
	if again, _ := c.Get1(ctx, "a"); again.Name != nameOrig {
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
	if again, _ := c.Get1(ctx, "a"); again.Name != nameOrig {
		t.Fatalf("scan exposed engine state: %q", again.Name)
	}
}

func testUniqueIndex(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	c := meta.NewCollection(s, nsAlpha, "records",
		meta.WithUnique("name", func(r *record) string { return r.Name }))

	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &record{Name: "shared"})
	})
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "b", &record{Name: "shared"})
	}); !errors.Is(err, meta.ErrConflict) {
		t.Fatalf("unique collision: got %v, want ErrConflict", err)
	}

	// Sparse: empty keys are unindexed and never collide.
	update(t, s, nsAlpha, func(w meta.Writer) error {
		if err := c.Insert(ctx, w, "u1", &record{Name: ""}); err != nil {
			return err
		}
		return c.Insert(ctx, w, "u2", &record{Name: ""})
	})

	id, rec, err := find(ctx, s, c, "name", "shared")
	if err != nil || id != "a" || rec == nil {
		t.Fatalf("find: %q, %+v, %v", id, rec, err)
	}

	// Replace rekeys; the old key is released, the new key claimed.
	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Replace(ctx, w, "a", &record{Name: "renamed"})
	})
	if _, _, err := find(ctx, s, c, "name", "shared"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("old key still resolves: %v", err)
	}
	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Insert(ctx, w, "b", &record{Name: "shared"})
	})

	// Delete releases the key.
	update(t, s, nsAlpha, func(w meta.Writer) error { return c.Delete(ctx, w, "b") })
	update(t, s, nsAlpha, func(w meta.Writer) error {
		return c.Insert(ctx, w, "b2", &record{Name: "shared"})
	})
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
	if got, err := c.Get1(ctx, "a"); err != nil || got.N != 1 {
		t.Fatalf("relaxed write not visible: %+v, %v", got, err)
	}
}

func testForcedRetry(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := ForcedRetry(factory(t, []string{nsAlpha}))
	c := meta.NewCollection[record](s, nsAlpha, "records")
	l := meta.NewLog[record](s, nsAlpha, "log")

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
		if _, err := l.Append(ctx, w, record{Name: "event"}); err != nil {
			return err
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
		n := 0
		if err := l.Scan(ctx, r, 0, func(meta.Seq, record) error { n++; return nil }); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("committed %d log entries, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testLogSeq(t *testing.T, factory Factory) {
	ctx := t.Context()
	s := factory(t, []string{nsAlpha})
	l := meta.NewLog[record](s, nsAlpha, "log")

	// seqs collects only committed numbers, published after each transaction
	// returns — appending inside the closure would break under retry (clause 1).
	var seqs []meta.Seq
	for i := range 3 {
		var seq meta.Seq
		update(t, s, nsAlpha, func(w meta.Writer) error {
			var err error
			seq, err = l.Append(ctx, w, record{N: i})
			return err
		})
		seqs = append(seqs, seq)
	}
	// A rolled-back append may reuse or burn its number — both are legal; only
	// committed entries must stay unique and increasing.
	boom := errors.New("boom")
	if err := s.Update(ctx, meta.Scope{Write: nsAlpha}, meta.CommitDurable, func(w meta.Writer) error {
		if _, err := l.Append(ctx, w, record{N: 99}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("rollback: %v", err)
	}
	var last meta.Seq
	update(t, s, nsAlpha, func(w meta.Writer) error {
		var err error
		last, err = l.Append(ctx, w, record{N: 3})
		return err
	})
	seqs = append(seqs, last)

	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("committed seqs not strictly increasing: %v", seqs)
		}
	}
	// Scan(after) is exclusive and resumes without duplication or loss.
	var all []int
	view(t, s, nsAlpha, func(r meta.Reader) error {
		return l.Scan(ctx, r, 0, func(_ meta.Seq, e record) error { all = append(all, e.N); return nil })
	})
	if len(all) != 4 || all[3] != 3 {
		t.Fatalf("scan all: %v", all)
	}
	var resumed []int
	view(t, s, nsAlpha, func(r meta.Reader) error {
		return l.Scan(ctx, r, seqs[1], func(_ meta.Seq, e record) error { resumed = append(resumed, e.N); return nil })
	})
	if len(resumed) != 2 || resumed[0] != 2 || resumed[1] != 3 {
		t.Fatalf("scan after %d: %v", seqs[1], resumed)
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
	if got, _ := c.Get1(ctx, "a"); got.N != 2 {
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

func view(t *testing.T, s meta.Store, ns string, fn func(meta.Reader) error) {
	t.Helper()
	if err := s.View(t.Context(), []string{ns}, fn); err != nil {
		t.Fatal(err)
	}
}

func find[R any](ctx context.Context, s meta.Store, c *meta.Collection[R], index, value string) (id string, rec *R, err error) {
	verr := s.View(ctx, []string{nsAlpha}, func(r meta.Reader) error {
		id, rec, err = c.Find(ctx, r, index, value)
		return nil
	})
	if verr != nil {
		return "", nil, verr
	}
	return id, rec, err
}
