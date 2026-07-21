package metalog

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/metering"
)

func TestEmitScan(t *testing.T) {
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
	var last meta.Seq
	err = r.Scan(t.Context(), 0, func(seq meta.Seq, e *metering.Entry) error {
		kinds = append(kinds, e.Kind)
		last = seq
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != metering.KindVMComputeStart || kinds[1] != metering.KindVMComputeStop {
		t.Fatalf("scan kinds: %v", kinds)
	}

	kinds = nil
	if err := r.Scan(t.Context(), last, func(meta.Seq, *metering.Entry) error {
		kinds = append(kinds, "dup")
		return nil
	}); err != nil {
		t.Fatalf("scan after last: %v", err)
	}
	if len(kinds) != 0 {
		t.Fatalf("scan after last seq must be empty, got %v", kinds)
	}
}
