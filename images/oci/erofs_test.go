package oci

import "testing"

func TestErofsVersionAtLeast(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"current", "mkfs.erofs (erofs-utils) 1.8.10\navailable compressors: lz4, lz4hc", false},
		{"floor exact", "mkfs.erofs (erofs-utils) 1.8", false},
		{"double digit minor", "mkfs.erofs (erofs-utils) 1.10.0", false},
		{"next major", "mkfs.erofs (erofs-utils) 2.0", false},
		{"ubuntu 24.04 stock", "mkfs.erofs (erofs-utils) 1.7.1", true},
		{"ancient", "mkfs.erofs (erofs-utils) 1.5", true},
		{"unparseable", "command not found", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := erofsVersionAtLeast(tt.output)
			if (err != nil) != tt.wantErr {
				t.Errorf("erofsVersionAtLeast(%q) = %v, wantErr %v", tt.output, err, tt.wantErr)
			}
		})
	}
}
