package metering

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestEntryJSONRoundTrip(t *testing.T) {
	in := Entry{
		EmittedAt:  time.Date(2026, 5, 20, 1, 2, 3, 0, time.UTC),
		Kind:       KindVMComputeStart,
		VMID:       "vm1",
		Reason:     ReasonBoot,
		Hypervisor: "ch",
		Shape:      Shape{CPU: 4, MemBytes: 1 << 30, StorageBytes: 10 << 30},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Entry
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip diverged:\n got: %#v\nwant: %#v", out, in)
	}
}

func TestKindWireFormat(t *testing.T) {
	// Wire-format strings are consumed by external BQ schema; renaming any
	// of these is a breaking change for downstream consumers.
	cases := []struct {
		got, want string
	}{
		{string(KindVMComputeStart), "vm.compute.start"},
		{string(KindVMComputeStop), "vm.compute.stop"},
		{string(KindVMStorageStart), "vm.storage.start"},
		{string(KindVMStorageStop), "vm.storage.stop"},
		{string(KindSnapStorageStart), "snap.storage.start"},
		{string(KindSnapStorageStop), "snap.storage.stop"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestNopRecorder(t *testing.T) {
	var r NopRecorder
	r.Emit(t.Context(), Entry{Kind: KindVMComputeStart, VMID: "x"})
}

func TestEntryWriteToProducesJSONLine(t *testing.T) {
	e := Entry{Kind: KindVMComputeStart, VMID: "vm1", Shape: Shape{CPU: 2}}
	var buf bytes.Buffer
	n, err := e.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if int(n) != buf.Len() {
		t.Errorf("n=%d, want %d", n, buf.Len())
	}
	if buf.Bytes()[buf.Len()-1] != '\n' {
		t.Errorf("missing trailing newline; got %q", buf.String())
	}
	var got Entry
	if err := json.Unmarshal(buf.Bytes()[:buf.Len()-1], &got); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	if got.Kind != e.Kind || got.VMID != e.VMID || got.Shape.CPU != e.Shape.CPU {
		t.Errorf("got %+v, want %+v", got, e)
	}
}

func TestEntryWriteToPropagatesWriterError(t *testing.T) {
	sentinel := errors.New("boom")
	e := Entry{Kind: KindVMComputeStart, VMID: "vm1"}
	_, err := e.WriteTo(errWriter{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Errorf("got err %v, want sentinel %v", err, sentinel)
	}
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
