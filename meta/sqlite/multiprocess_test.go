package sqlite

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

const (
	mpOps      = 15
	killTxnLen = 5
	killRounds = 8

	inverseWorkers = 8
	inverseOps     = 10
)

// TestMultiProcessCorrectness is §9's real-process storm for the sqlite
// engine: acknowledged inserts and their name rows all present, names
// referentially consistent, zero corruption on reopen.
func TestMultiProcessCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	_ = newStore(t, dir, "alpha") // parent initializes once; workers only open
	workers := stormWorkers()
	cmds := contracttest.SpawnWorkers(t, workers, "TestMultiProcessWorker", func(w int) []string {
		return []string{"META_MP_DIR=" + dir, "META_MP_WORKER=" + strconv.Itoa(w), fmt.Sprintf("META_MP_OPS=%d", mpOps)}
	})
	contracttest.WaitWorkers(t, cmds, "failed")

	s := newStore(t, dir, "alpha")
	records := map[string]struct{}{}
	names := map[string]string{}
	err := s.View(t.Context(), []string{"alpha"}, func(r meta.Reader) error {
		if err := r.ScanRaw(t.Context(), "alpha", "records", func(id string, _ json.RawMessage) error {
			records[id] = struct{}{}
			return nil
		}); err != nil {
			return err
		}
		return r.ScanRaw(t.Context(), "alpha", "names", func(name string, raw json.RawMessage) error {
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				return err
			}
			names[name] = id
			return nil
		})
	})
	if err != nil {
		t.Fatalf("reopen after storm: %v", err)
	}
	if want := workers * mpOps; len(records) != want || len(names) != want {
		t.Fatalf("lost writes: %d records / %d names, want %d", len(records), len(names), want)
	}
	for name, id := range names {
		if _, ok := records[id]; !ok {
			t.Fatalf("name %s points at missing record %s", name, id)
		}
	}
	integrityCheck(t, dir)
}

// TestMultiProcessWorker is the helper-process body.
func TestMultiProcessWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	worker := os.Getenv("META_MP_WORKER")
	ops, err := strconv.Atoi(os.Getenv("META_MP_OPS"))
	if err != nil {
		t.Fatal(err)
	}
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	for i := range ops {
		id := fmt.Sprintf("w%s-op%d", worker, i)
		updateRetry(t, s, meta.Scope{Write: "alpha"}, func(w meta.Writer) error {
			if err := w.PutRaw(ctx, "alpha", "records", id, json.RawMessage(`{"n":`+strconv.Itoa(i)+`}`), false); err != nil {
				return err
			}
			return w.PutRaw(ctx, "alpha", "names", "name-"+id, json.RawMessage(`"`+id+`"`), false)
		})
	}
}

// TestInverseScopeNoDeadlock storms mutually-inverse scopes across processes.
func TestInverseScopeNoDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	_ = newStore(t, dir, "alpha", "beta")
	scopes := []string{"alpha:beta", "beta:alpha"}
	cmds := contracttest.SpawnWorkers(t, 2*inverseWorkers, "TestInverseScopeWorker", func(w int) []string {
		return []string{"META_MP_DIR=" + dir, "META_MP_WORKER=" + strconv.Itoa(w), "META_MP_SCOPE=" + scopes[w%2]}
	})
	contracttest.WaitWorkers(t, cmds, "failed (deadlock or error)")
	s := newStore(t, dir, "alpha", "beta")
	for _, ns := range []string{"alpha", "beta"} {
		total := 0
		if err := s.View(t.Context(), []string{ns}, func(r meta.Reader) error {
			return r.ScanRaw(t.Context(), ns, "records", func(string, json.RawMessage) error {
				total++
				return nil
			})
		}); err != nil {
			t.Fatalf("reopen %s: %v", ns, err)
		}
		if want := inverseWorkers * inverseOps; total != want {
			t.Fatalf("%s lost writes: %d records, want %d", ns, total, want)
		}
	}
}

// TestInverseScopeWorker is the helper-process body.
func TestInverseScopeWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	worker := os.Getenv("META_MP_WORKER")
	write, read, _ := strings.Cut(os.Getenv("META_MP_SCOPE"), ":")
	s := newStore(t, dir, "alpha", "beta")
	ctx := t.Context()
	for i := range inverseOps {
		id := fmt.Sprintf("w%s-op%d", worker, i)
		updateRetry(t, s, meta.Scope{Write: write, Read: []string{read}}, func(w meta.Writer) error {
			seen := 0
			if err := w.ScanRaw(ctx, read, "records", func(string, json.RawMessage) error {
				seen++
				return nil
			}); err != nil {
				return err
			}
			return w.PutRaw(ctx, write, "records", id, json.RawMessage(`{"peer":`+strconv.Itoa(seen)+`}`), false)
		})
	}
}

// TestKillStormAtomicity is §9's commit-atomicity-under-crash gate for the
// sqlite engine: SIGKILL sampled across WAL-append and commit-frame windows;
// every multi-record transaction must reopen wholly applied or wholly absent,
// every acked transaction wholly applied.
func TestKillStormAtomicity(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	dir := t.TempDir()
	_ = newStore(t, dir, "alpha")
	acked := map[string]struct{}{}
	for round := range killRounds {
		cmd := exec.Command(os.Args[0], "-test.run=TestKillStormWorker$", "-test.count=1") //nolint:gosec
		cmd.Env = append(os.Environ(), "META_MP_DIR="+dir, "META_MP_WORKER="+strconv.Itoa(round))
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(pipe)
		got := 0
		for sc.Scan() {
			if id, ok := strings.CutPrefix(sc.Text(), "ACK "); ok {
				acked[id] = struct{}{}
				got++
				// Kill at a round-varied depth so the SIGKILL lands in
				// different WAL-append/commit windows.
				if got >= 3+round*2 {
					break
				}
			}
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill worker: %v", err)
		}
		_ = cmd.Wait()
	}

	s := newStore(t, dir, "alpha")
	byTxn := map[string]int{}
	err := s.View(t.Context(), []string{"alpha"}, func(r meta.Reader) error {
		return r.ScanRaw(t.Context(), "alpha", "records", func(id string, _ json.RawMessage) error {
			txn, _, ok := strings.Cut(id, "/")
			if !ok {
				return fmt.Errorf("malformed id %s", id)
			}
			byTxn[txn]++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("reopen after kill storm: %v", err)
	}
	for txn, n := range byTxn {
		if n != killTxnLen {
			t.Fatalf("transaction %s partially applied: %d of %d rows", txn, n, killTxnLen)
		}
	}
	for txn := range acked {
		if byTxn[txn] != killTxnLen {
			t.Fatalf("acked durable transaction %s lost or partial: %d rows", txn, byTxn[txn])
		}
	}
	integrityCheck(t, dir)
}

// TestKillStormWorker writes multi-record durable transactions and acks each.
func TestKillStormWorker(t *testing.T) {
	dir := os.Getenv("META_MP_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	round := os.Getenv("META_MP_WORKER")
	s := newStore(t, dir, "alpha")
	ctx := t.Context()
	for i := range 1000 {
		txn := fmt.Sprintf("r%s-t%d", round, i)
		updateRetry(t, s, meta.Scope{Write: "alpha"}, func(w meta.Writer) error {
			for k := range killTxnLen {
				id := fmt.Sprintf("%s/%d", txn, k)
				if err := w.PutRaw(ctx, "alpha", "records", id, json.RawMessage(`{"k":`+strconv.Itoa(k)+`}`), false); err != nil {
					return err
				}
			}
			return nil
		})
		fmt.Printf("ACK %s\n", txn)
	}
}

// updateRetry retries on ErrBusy — the mapped, retryable-by-contract outcome
// under extreme oversubscription (§4); reconciliation still demands exact counts.
func updateRetry(t *testing.T, s *Store, sc meta.Scope, fn func(meta.Writer) error) {
	t.Helper()
	for {
		err := s.Update(t.Context(), sc, meta.CommitDurable, fn)
		if err == nil {
			return
		}
		if !errors.Is(err, meta.ErrBusy) {
			t.Fatalf("update: %v", err)
		}
	}
}

func stormWorkers() int {
	if v, err := strconv.Atoi(os.Getenv("COCOON_STORM_WORKERS")); err == nil && v > 0 {
		return v
	}
	return 256
}

func integrityCheck(t *testing.T, dir string) {
	t.Helper()
	db, err := open(filepath.Join(dir, DBFileName), "FULL", false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity check: %q err=%v", result, err)
	}
}
