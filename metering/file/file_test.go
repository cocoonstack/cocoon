package file

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/metering"
)

func TestRecorderRoundTrip(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	r, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.Emit(ctx, metering.Entry{
		EmittedAt: now,
		Kind:      metering.KindVMComputeStart,
		VMID:      "vm1",
		Reason:    metering.ReasonBoot,
		Shape:     metering.Shape{CPU: 4, MemBytes: 1 << 30},
	})
	r.Emit(ctx, metering.Entry{
		EmittedAt: now.Add(time.Second),
		Kind:      metering.KindVMComputeStop,
		VMID:      "vm1",
		Reason:    metering.ReasonStopUser,
	})

	got := readEntries(t, path)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Kind != metering.KindVMComputeStart || got[1].Kind != metering.KindVMComputeStop {
		t.Errorf("got kinds %v %v", got[0].Kind, got[1].Kind)
	}
	if got[0].Shape.CPU != 4 {
		t.Errorf("got CPU=%d, want 4", got[0].Shape.CPU)
	}
}

func TestRecorderConcurrent(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	r, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const N = 200
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			r.Emit(ctx, metering.Entry{Kind: metering.KindVMComputeStart, VMID: "vm", Shape: metering.Shape{CPU: i}})
		})
	}
	wg.Wait()

	got := readEntries(t, path)
	if len(got) != N {
		t.Errorf("got %d lines, want %d", len(got), N)
	}
}

func TestNewMissingParentDirErrors(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "missing-subdir", "ledger.jsonl"))
	if err == nil {
		t.Error("expected error opening ledger in missing dir; got nil")
	}
}

func readEntries(t *testing.T, path string) []metering.Entry {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close() //nolint:errcheck

	var out []metering.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e metering.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
