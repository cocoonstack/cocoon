package core

import (
	"fmt"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/metering"
	meteringfile "github.com/cocoonstack/cocoon/metering/file"
	meteringstderr "github.com/cocoonstack/cocoon/metering/stderr"
)

func TestBuildRecorderBackends(t *testing.T) {
	tests := []struct {
		name    string
		backend config.MeteringBackend
		want    metering.Recorder
	}{
		{"empty defaults to file", "", (*meteringfile.Recorder)(nil)},
		{"explicit file", config.MeteringFile, (*meteringfile.Recorder)(nil)},
		{"nop", config.MeteringNop, metering.NopRecorder{}},
		{"stderr", config.MeteringStderr, (*meteringstderr.Recorder)(nil)},
		{"unknown falls back to nop", "kafka", metering.NopRecorder{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &config.Config{
				RootDir:  t.TempDir(),
				Metering: config.MeteringConfig{Backend: tt.backend},
			}
			got := buildRecorder(t.Context(), conf)
			if gotT, wantT := fmt.Sprintf("%T", got), fmt.Sprintf("%T", tt.want); gotT != wantT {
				t.Errorf("backend=%q got %s, want %s", tt.backend, gotT, wantT)
			}
		})
	}
}

func TestBuildFileRecorderCustomPath(t *testing.T) {
	dir := t.TempDir()
	conf := &config.Config{
		RootDir: dir,
		Metering: config.MeteringConfig{
			Backend: config.MeteringFile,
			File:    config.FileMeteringConfig{Path: dir + "/custom/ledger.jsonl"},
		},
	}
	r := buildRecorder(t.Context(), conf)
	if _, ok := r.(*meteringfile.Recorder); !ok {
		t.Fatalf("got %T, want *meteringfile.Recorder", r)
	}
}
