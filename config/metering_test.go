package config

import (
	"strings"
	"testing"
)

func TestMeteringConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		backend MeteringBackend
		wantErr bool
	}{
		{"empty defaults", "", false},
		{"file", MeteringFile, false},
		{"nop", MeteringNop, false},
		{"stderr", MeteringStderr, false},
		{"unknown", "kafka", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MeteringConfig{Backend: tt.backend}.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%q) err = %v, wantErr=%v", tt.backend, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "metering.backend") {
				t.Errorf("err = %v; want containing 'metering.backend'", err)
			}
		})
	}
}
