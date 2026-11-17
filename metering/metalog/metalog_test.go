package metalog

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/metering"
)

func TestEmitSurvivesACanceledContext(t *testing.T) {
	s := openStore(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	New(s).Emit(ctx, metering.Entry{Kind: metering.KindVMStorageStop, VMID: "vm1"})

	if entries := scanEntries(t, s); len(entries) != 1 {
		t.Fatalf("entries = %d, want the stop event kept after SIGTERM canceled the context", len(entries))
	}
}

func TestEmitAppendsInOrder(t *testing.T) {
	s := openStore(t)

	r := New(s)
	r.Emit(t.Context(), metering.Entry{Kind: metering.KindVMComputeStart, VMID: "vm1"})
	r.Emit(t.Context(), metering.Entry{Kind: metering.KindVMComputeStop, VMID: "vm1"})

	entries := scanEntries(t, s)
	if len(entries) != 2 || entries[0].Kind != metering.KindVMComputeStart || entries[1].Kind != metering.KindVMComputeStop {
		t.Fatalf("appended entries: %v", entries)
	}
}

func openStore(t *testing.T) *metajson.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := metajson.Open(metajson.Namespace{
		Name:     NamespaceName,
		FilePath: filepath.Join(dir, "log.json"),
		LockPath: filepath.Join(dir, "log.lock"),
		Codec:    metajson.GenericCodec{},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func scanEntries(t *testing.T, s meta.Store) []metering.Entry {
	t.Helper()
	var entries []metering.Entry
	err := s.View(t.Context(), []string{NamespaceName}, func(rd meta.Reader) error {
		return rd.ScanRaw(t.Context(), NamespaceName, TableEntries, func(id string, raw jsontext.Value) error {
			if _, err := strconv.ParseUint(id, 10, 64); err != nil {
				return nil
			}
			var e metering.Entry
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return entries
}
