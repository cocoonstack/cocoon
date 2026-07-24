//go:build linux

package bridge

import "testing"

func TestParseTAPName(t *testing.T) {
	tests := []struct {
		name       string
		wantPrefix string
		wantOK     bool
	}{
		{name: "bt12345678-0", wantPrefix: "12345678", wantOK: true},
		{name: "bt12345678-1", wantPrefix: "12345678", wantOK: true},
		{name: "btabc-3", wantPrefix: "abc", wantOK: true},
		{name: "btabc-def-5", wantPrefix: "abc-def", wantOK: true},

		{name: "wrong-prefix-0"},
		{name: "bt"},
		{name: "bt-0"}, // empty prefix
		{name: "bt12345678"},
		{name: ""},
	}
	for _, tt := range tests {
		label := tt.name
		if label == "" {
			label = "<empty>"
		}
		t.Run(label, func(t *testing.T) {
			gotPrefix, gotOK := parseTAPName(tt.name)
			if gotOK != tt.wantOK {
				t.Errorf("parseTAPName(%q) ok = %v, want %v", tt.name, gotOK, tt.wantOK)
			}
			if gotPrefix != tt.wantPrefix {
				t.Errorf("parseTAPName(%q) prefix = %q, want %q", tt.name, gotPrefix, tt.wantPrefix)
			}
		})
	}
}
