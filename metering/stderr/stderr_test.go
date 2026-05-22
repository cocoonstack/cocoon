package stderr

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/metering"
)

func TestRecorderEmitsJSONLine(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{out: &buf}
	r.Emit(t.Context(), metering.Entry{
		Kind:       metering.KindVMComputeStart,
		VMID:       "vm1",
		Hypervisor: "test",
		Shape:      metering.Shape{CPU: 2},
	})
	line := strings.TrimRight(buf.String(), "\n")
	var got metering.Entry
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("emitted line is not valid JSON %q: %v", line, err)
	}
	if got.Kind != metering.KindVMComputeStart || got.VMID != "vm1" {
		t.Errorf("got %+v", got)
	}
}

func TestRecorderConcurrent(t *testing.T) {
	var buf bytes.Buffer
	r := &Recorder{out: &buf}
	const N = 100
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			r.Emit(t.Context(), metering.Entry{Kind: metering.KindVMComputeStart, Shape: metering.Shape{CPU: i}})
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != N {
		t.Errorf("got %d lines, want %d", len(lines), N)
	}
}
