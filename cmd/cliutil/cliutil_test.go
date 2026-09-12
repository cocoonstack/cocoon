package cliutil

import "testing"

func TestValidateFormat(t *testing.T) {
	tests := []struct {
		format  string
		wantErr string
	}{
		{"table", ""},
		{"json", ""},
		{"", ""},
		{"yaml", `--format "yaml" is invalid: want "table" or "json"`},
		{"JSON", `--format "JSON" is invalid: want "table" or "json"`},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			err := ValidateFormat(tt.format)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ValidateFormat(%q) = %v, want nil", tt.format, err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("ValidateFormat(%q) = nil, want %q", tt.format, tt.wantErr)
			case tt.wantErr != "" && err.Error() != tt.wantErr:
				t.Fatalf("ValidateFormat(%q) = %q, want %q", tt.format, err, tt.wantErr)
			}
		})
	}
}
