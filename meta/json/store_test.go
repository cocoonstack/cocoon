package json

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

type crashPoint struct {
	step string
	err  error
}

func (c *crashPoint) hook(step string) error {
	if step == c.step {
		return c.err
	}
	return nil
}

func newStore(t *testing.T, dir string, nss ...string) *Store {
	t.Helper()
	defs := make([]Namespace, 0, len(nss))
	for _, ns := range nss {
		defs = append(defs, Namespace{
			Name:     ns,
			FilePath: filepath.Join(dir, ns+".json"),
			LockPath: filepath.Join(dir, ns+".lock"),
			Codec:    GenericCodec{},
		})
	}
	s, err := Open(defs...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return newStore(t, t.TempDir(), nss...)
	})
}

func TestContractForcedRetryEngine(t *testing.T) {
	// The suite must also hold when EVERY closure is double-run.
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return contracttest.ForcedRetry(newStore(t, t.TempDir(), nss...))
	})
}

func TestCommitAtomicityUnderCrash(t *testing.T) {
	steps := []string{"prev-linked", "prev-rotated", "main-renamed", "main-synced", "prev-synced"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			s := newStore(t, dir, "alpha")
			c := meta.NewCollection[map[string]int](s, "alpha", "records")

			seed := map[string]int{"v": 1}
			if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
				return c.Insert(ctx, w, "a", &seed)
			}); err != nil {
				t.Fatal(err)
			}

			crash := errors.New("crash")
			testCrashStep = (&crashPoint{step: step, err: crash}).hook
			defer func() { testCrashStep = nil }()
			err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
				next := map[string]int{"v": 2}
				if err := c.Replace(ctx, w, "a", &next); err != nil {
					return err
				}
				fresh := map[string]int{"v": 2}
				return c.Insert(ctx, w, "b", &fresh)
			})
			if !errors.Is(err, crash) {
				t.Fatalf("crash injection: %v", err)
			}
			testCrashStep = nil

			// Reopen as a fresh process would and require wholly-old or wholly-new.
			s2 := newStore(t, dir, "alpha")
			c2 := meta.NewCollection[map[string]int](s2, "alpha", "records")
			var a, b *map[string]int
			if err := s2.View(ctx, []string{"alpha"}, func(r meta.Reader) error {
				var err error
				if a, err = c2.Get(ctx, r, "a"); err != nil {
					return err
				}
				b, err = c2.Get(ctx, r, "b")
				if errors.Is(err, meta.ErrNotFound) {
					b = nil
					return nil
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			switch {
			case (*a)["v"] == 1 && b == nil:
			case (*a)["v"] == 2 && b != nil && (*b)["v"] == 2:
			default:
				t.Fatalf("torn state after crash at %s: a=%v b=%v", step, *a, b)
			}
		})
	}
}

func TestPrevRecovery(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
	path := filepath.Join(dir, "alpha.json")

	for gen := 1; gen <= 2; gen++ {
		v := map[string]int{"gen": gen}
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			if gen == 1 {
				return c.Insert(ctx, w, "a", &v)
			}
			return c.Replace(ctx, w, "a", &v)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, dir, "alpha")
	c2 := meta.NewCollection[map[string]int](s2, "alpha", "records")
	got, err := c2.Get1(ctx, "a")
	if err != nil || (*got)["gen"] != 1 {
		t.Fatalf("recovered generation: %v, %v", got, err)
	}

	// A commit over a recovered main must not rotate the only good generation away.
	v3 := map[string]int{"gen": 3}
	if err := s2.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c2.Replace(ctx, w, "a", &v3)
	}); err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(path + prevSuffix)
	if err != nil {
		t.Fatal(err)
	}
	m, err := GenericCodec{}.Decode(prev)
	if err != nil {
		t.Fatalf(".prev rotated in the torn generation: %v", err)
	}
	raw, ok := m.Get("records", "a")
	if !ok || string(raw) != `{"gen":1}` {
		t.Fatalf(".prev lost the good generation: %s", raw)
	}
}

func TestCorruptBothGenerations(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	path := filepath.Join(dir, "alpha.json")
	if err := os.WriteFile(path, []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := s.View(ctx, []string{"alpha"}, func(meta.Reader) error { return nil })
	if !errors.Is(err, meta.ErrCorrupt) {
		t.Fatalf("both generations bad: got %v, want ErrCorrupt", err)
	}
}

func TestRawViewSkipsLock(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
	v := map[string]int{"v": 1}
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &v)
	}); err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_ = s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(meta.Writer) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { <-holderDone }()
	defer close(release)

	done := make(chan error, 1)
	go func() {
		done <- s.RawView(ctx, "alpha", func(r meta.Reader) error {
			_, err := c.Get(ctx, r, "a")
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RawView blocked behind a held namespace lock")
	}
}

func TestLockedUpdateSkipsRotation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int](s, "alpha", "records")
	path := filepath.Join(dir, "alpha.json")

	for gen := 1; gen <= 2; gen++ {
		v := map[string]int{"gen": gen}
		if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			if gen == 1 {
				return c.Insert(ctx, w, "a", &v)
			}
			return c.Replace(ctx, w, "a", &v)
		}); err != nil {
			t.Fatal(err)
		}
	}
	prevBefore, err := os.ReadFile(path + prevSuffix)
	if err != nil {
		t.Fatal(err)
	}

	locker, err := s.NamespaceLocker("alpha")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := locker.TryLock(ctx)
	if err != nil || !ok {
		t.Fatalf("trylock: %v %v", ok, err)
	}
	defer locker.Unlock(ctx) //nolint:errcheck
	v3 := map[string]int{"gen": 3}
	if err := s.LockedUpdate(ctx, "alpha", func(w meta.Writer) error {
		return c.Replace(ctx, w, "a", &v3)
	}); err != nil {
		t.Fatal(err)
	}
	prevAfter, err := os.ReadFile(path + prevSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if string(prevBefore) != string(prevAfter) {
		t.Fatal("LockedUpdate rotated .prev; legacy WriteRaw must not")
	}
	// Still holding the namespace lock: a plain View would self-deadlock — the
	// exact hazard the legacy adapter exists for.
	var got *map[string]int
	if err := s.LockedView(ctx, "alpha", func(r meta.Reader) error {
		var verr error
		got, verr = c.Get(ctx, r, "a")
		return verr
	}); err != nil || (*got)["gen"] != 3 {
		t.Fatalf("locked update lost: %v %v", got, err)
	}
}

func TestNamespaceLockerExcludesTransactions(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int](s, "alpha", "records")

	locker, err := s.NamespaceLocker("alpha")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := locker.TryLock(ctx)
	if err != nil || !ok {
		t.Fatalf("trylock: %v %v", ok, err)
	}
	blocked := make(chan error, 1)
	go func() {
		v := map[string]int{}
		blocked <- s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
			return c.Insert(ctx, w, "a", &v)
		})
	}()
	select {
	case err := <-blocked:
		t.Fatalf("transaction ran despite held namespace lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := locker.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
}

func TestExternalProcessEvents(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")

	ch, release, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// A second store over the same files stands in for an external process.
	s2 := newStore(t, dir, "alpha")
	c2 := meta.NewCollection[map[string]int](s2, "alpha", "records")
	v := map[string]int{"v": 1}
	if err := s2.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c2.Insert(ctx, w, "a", &v)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no event after external commit")
	}
}

func TestGenericCodecRoundTrip(t *testing.T) {
	m := NewModel()
	m.Put("records", "b", []byte(`{"n":2}`))
	m.Put("records", "a", []byte(`{"n":1}`))
	m.SetSeq("log", 7)
	data, err := GenericCodec{}.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := GenericCodec{}.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := GenericCodec{}.Encode(m2)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(data2) {
		t.Fatalf("round trip unstable:\n%s\n%s", data, data2)
	}
	if m2.Seq("log") != 7 {
		t.Fatalf("seq lost: %d", m2.Seq("log"))
	}
	if _, ok := m2.Get("records", "a"); !ok {
		t.Fatal("record lost")
	}
}

func TestOpenValidation(t *testing.T) {
	if _, err := Open(Namespace{Name: "x"}); err == nil {
		t.Fatal("incomplete namespace accepted")
	}
	dir := t.TempDir()
	def := Namespace{Name: "x", FilePath: filepath.Join(dir, "x.json"), LockPath: filepath.Join(dir, "x.lock"), Codec: GenericCodec{}}
	if _, err := Open(def, def); err == nil {
		t.Fatal("duplicate namespace accepted")
	}
}
