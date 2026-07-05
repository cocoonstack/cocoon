package hypervisor

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestResolveForRestoreStates(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	tests := []struct {
		name    string
		state   types.VMState
		wantErr string
	}{
		{"running restorable", types.VMStateRunning, ""},
		{"stopped restorable (hibernate resume)", types.VMStateStopped, ""},
		{"error rejected", types.VMStateError, "must be running or stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "vm-" + string(tt.state)
			seedVMRecord(t, b, id, 1, 512, 1024, true)
			if err := b.DB.Update(t.Context(), func(idx *VMIndex) error {
				idx.VMs[id].State = tt.state
				return nil
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}

			_, rec, err := b.ResolveForRestore(t.Context(), id)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveForRestore: %v", err)
				}
				if rec.State != tt.state {
					t.Errorf("state %s, want %s", rec.State, tt.state)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want %q", err, tt.wantErr)
			}
		})
	}
}
