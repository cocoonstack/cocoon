//go:build linux

package bridge

import "testing"

func TestParseTAPName(t *testing.T) {
	tests := []struct {
		tapPrefix  string
		name       string
		wantPrefix string
		wantOK     bool
	}{
		{tapPrefix: "bt", name: "bt12345678-0", wantPrefix: "12345678", wantOK: true},
		{tapPrefix: "bt", name: "bt12345678-1", wantPrefix: "12345678", wantOK: true},
		{tapPrefix: "bt", name: "btabc-3", wantPrefix: "abc", wantOK: true},
		{tapPrefix: "bt", name: "btabc-def-5", wantPrefix: "abc-def", wantOK: true},
		{tapPrefix: "mt", name: "mt12345678-0", wantPrefix: "12345678", wantOK: true},

		{tapPrefix: "bt", name: "wrong-prefix-0"},
		{tapPrefix: "bt", name: "bt"},
		{tapPrefix: "bt", name: "bt-0"},
		{tapPrefix: "bt", name: "bt12345678"},
		{tapPrefix: "bt", name: ""},
		{tapPrefix: "mt", name: "bt12345678-0"},
	}
	for _, tt := range tests {
		label := tt.tapPrefix + "/" + tt.name
		t.Run(label, func(t *testing.T) {
			gotPrefix, gotOK := parseTAPName(tt.tapPrefix, tt.name)
			if gotOK != tt.wantOK {
				t.Errorf("parseTAPName(%q, %q) ok = %v, want %v", tt.tapPrefix, tt.name, gotOK, tt.wantOK)
			}
			if gotPrefix != tt.wantPrefix {
				t.Errorf("parseTAPName(%q, %q) prefix = %q, want %q", tt.tapPrefix, tt.name, gotPrefix, tt.wantPrefix)
			}
		})
	}
}
