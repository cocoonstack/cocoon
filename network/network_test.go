package network

import (
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestAddRecover(t *testing.T) {
	existing := []*types.NetworkConfig{
		{TAP: "tap0", NumQueues: 2},
		nil,
		{TAP: "tap2", NumQueues: 4},
	}
	specs := AddRecover(existing)
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
	if specs[0].Queues != 2 || specs[2].Queues != 4 {
		t.Errorf("persisted queue counts not restored: %+v", specs)
	}
	if specs[1].Queues != 0 || specs[1].Existing != nil {
		t.Errorf("nil NIC must keep the derive-from-CPU default: %+v", specs[1])
	}
	if specs[1].Index != 1 {
		t.Errorf("index not preserved for nil entry: %+v", specs[1])
	}
}

func TestAddRange(t *testing.T) {
	specs := AddRange(2, 3)
	if len(specs) != 3 || specs[0].Index != 2 || specs[2].Index != 4 {
		t.Fatalf("got %+v", specs)
	}
	for _, s := range specs {
		if s.Queues != 0 || s.Existing != nil {
			t.Errorf("fresh spec must be zero beyond Index: %+v", s)
		}
	}
}

func TestParseTAPName(t *testing.T) {
	tests := []struct {
		tapPrefix  string
		name       string
		wantPrefix string
		wantOK     bool
	}{
		{tapPrefix: "bt", name: "btAB234567-0", wantPrefix: "AB234567", wantOK: true},
		{tapPrefix: "bt", name: "btAB234567-1", wantPrefix: "AB234567", wantOK: true},
		{tapPrefix: "bt", name: "btZZZZZZZZ-15", wantPrefix: "ZZZZZZZZ", wantOK: true},
		{tapPrefix: "mt", name: "mtAB234567-0", wantPrefix: "AB234567", wantOK: true},

		{tapPrefix: "qv", name: "qvb1a2b3c4d-5e"},
		{tapPrefix: "bt", name: "btabc-3"},
		{tapPrefix: "bt", name: "btabc-def-5"},
		{tapPrefix: "bt", name: "bt12345678-0"},
		{tapPrefix: "bt", name: "btab234567-0"},
		{tapPrefix: "bt", name: "btAB23456-0"},
		{tapPrefix: "bt", name: "btAB2345677-0"},
		{tapPrefix: "bt", name: "btAB234567-x"},
		{tapPrefix: "bt", name: "btAB234567-+1"},
		{tapPrefix: "bt", name: "btAB234567-01"},
		{tapPrefix: "bt", name: "btAB234567-0-1"},
		{tapPrefix: "bt", name: "btAB234567-"},
		{tapPrefix: "bt", name: "wrong-prefix-0"},
		{tapPrefix: "bt", name: "bt"},
		{tapPrefix: "bt", name: "bt-0"},
		{tapPrefix: "bt", name: "btAB234567"},
		{tapPrefix: "bt", name: ""},
		{tapPrefix: "mt", name: "btAB234567-0"},
	}
	for _, tt := range tests {
		label := tt.tapPrefix + "/" + tt.name
		t.Run(label, func(t *testing.T) {
			gotPrefix, gotOK := ParseTAPName(tt.tapPrefix, tt.name)
			if gotOK != tt.wantOK {
				t.Errorf("ParseTAPName(%q, %q) ok = %v, want %v", tt.tapPrefix, tt.name, gotOK, tt.wantOK)
			}
			if gotPrefix != tt.wantPrefix {
				t.Errorf("ParseTAPName(%q, %q) prefix = %q, want %q", tt.tapPrefix, tt.name, gotPrefix, tt.wantPrefix)
			}
		})
	}
}
