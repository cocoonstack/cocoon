package json

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

func TestContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return newStore(t, t.TempDir(), nss...)
	})
}

func TestContractForcedRetryEngine(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return contracttest.ForcedRetry(newStore(t, t.TempDir(), nss...))
	})
}

func TestCommitAtomicityUnderCrash(t *testing.T) {
	steps := []string{"prev-linked", "prev-rotated", "main-renamed", "main-synced", "prev-synced", "dir-synced"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			s := newStore(t, dir, "alpha")
			c := meta.NewCollection[map[string]int]("alpha", "records")

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

			s2 := newStore(t, dir, "alpha")
			c2 := meta.NewCollection[map[string]int]("alpha", "records")
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
	c := meta.NewCollection[map[string]int]("alpha", "records")
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
	c2 := meta.NewCollection[map[string]int]("alpha", "records")
	got, err := mustGet(t, s2, "alpha", "a")
	if err != nil || (*got)["gen"] != 1 {
		t.Fatalf("recovered generation: %v, %v", got, err)
	}

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

func TestExternalProcessEvents(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")

	ch, release, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	s2 := newStore(t, dir, "alpha")
	c2 := meta.NewCollection[map[string]int]("alpha", "records")
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

func TestEventsForcedOverflow(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	testWatchErrs = make(chan struct{})
	t.Cleanup(func() { testWatchErrs = nil })
	s := newStore(t, dir, "alpha")

	ch, release, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := s.events.watcher.Remove(dir); err != nil {
		t.Fatalf("remove watch: %v", err)
	}
	c := meta.NewCollection[map[string]int]("alpha", "records")
	v := map[string]int{"v": 1}
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &v)
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
	}

	testWatchErrs <- struct{}{}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no signal after forced overflow (safety poll is 5s, so this was the overflow path)")
	}
}

func TestDurableSyncOrder(t *testing.T) {
	var steps []string
	testCrashStep = func(step string) error { steps = append(steps, step); return nil }
	t.Cleanup(func() { testCrashStep = nil })
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")

	steps = nil
	v := map[string]int{"n": 1}
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &v)
	}); err != nil {
		t.Fatal(err)
	}
	idxOf := func(name string) int { return slices.Index(steps, name) }
	renamed, mainSync, prevSync, dirSync := idxOf("main-renamed"), idxOf("main-synced"), idxOf("prev-synced"), idxOf("dir-synced")
	if renamed < 0 || mainSync < 0 || prevSync < 0 || dirSync < 0 || !(renamed < mainSync && mainSync < prevSync && prevSync < dirSync) {
		t.Fatalf("durable ack without full sync order: %v", steps)
	}

	steps = nil
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitRelaxed, func(w meta.Writer) error {
		return c.Insert(ctx, w, "b", &v, meta.RelaxedOK)
	}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(steps, "prev-synced") || slices.Contains(steps, "dir-synced") {
		t.Fatalf("relaxed commit ran the durable sync tail: %v", steps)
	}
	if !slices.Contains(steps, "main-renamed") {
		t.Fatalf("relaxed commit skipped the atomic rename: %v", steps)
	}
}

func TestCleanUpdateSkipsCommit(t *testing.T) {
	var steps []string
	testCrashStep = func(step string) error { steps = append(steps, step); return nil }
	t.Cleanup(func() { testCrashStep = nil })
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")
	v := map[string]int{"n": 1}
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		return c.Insert(ctx, w, "a", &v)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}

	steps = nil
	if err := s.Update(ctx, meta.Scope{Write: "alpha"}, meta.CommitDurable, func(w meta.Writer) error {
		_, _, err := w.GetRaw(ctx, "alpha", "records", "a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("clean transaction ran the commit tail: %v", steps)
	}
	after, err := os.ReadFile(filepath.Join(dir, "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("clean transaction rewrote the namespace file")
	}
}

func TestTrailingDataFallsBackToPrev(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := newStore(t, dir, "alpha")
	c := meta.NewCollection[map[string]int]("alpha", "records")
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

	main, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(main, []byte("{}garbage")...), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := newStore(t, dir, "alpha")
	got, err := mustGet(t, s2, "alpha", "a")
	if err != nil || (*got)["gen"] != 1 {
		t.Fatalf("trailing-data main was served instead of .prev: %v, %v", got, err)
	}
}

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

func mustGet(t *testing.T, s *Store, ns, id string) (*map[string]int, error) {
	t.Helper()
	c := meta.NewCollection[map[string]int](ns, "records")
	var rec *map[string]int
	err := s.View(t.Context(), []string{ns}, func(r meta.Reader) error {
		var err error
		rec, err = c.Get(t.Context(), r, id)
		return err
	})
	return rec, err
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
