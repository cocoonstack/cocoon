package metalog

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/metering"
)

func TestEmitAppendsInOrder(t *testing.T) {
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
	defer s.Close() //nolint:errcheck

	r := New(s)
	r.Emit(t.Context(), metering.Entry{Kind: metering.KindVMComputeStart, VMID: "vm1"})
	r.Emit(t.Context(), metering.Entry{Kind: metering.KindVMComputeStop, VMID: "vm1"})

	var kinds []metering.Kind
	err = s.View(t.Context(), []string{NamespaceName}, func(rd meta.Reader) error {
		return rd.ScanRaw(t.Context(), NamespaceName, TableEntries, func(id string, raw json.RawMessage) error {
			if _, err := strconv.ParseUint(id, 10, 64); err != nil {
				return nil
			}
			var e metering.Entry
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			kinds = append(kinds, e.Kind)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != metering.KindVMComputeStart || kinds[1] != metering.KindVMComputeStop {
		t.Fatalf("appended kinds: %v", kinds)
	}
}
