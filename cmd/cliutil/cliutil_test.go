package cliutil

import (
	"testing"

	"github.com/spf13/cobra"
)

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
			got := ""
			if err := validateFormat(tt.format); err != nil {
				got = err.Error()
			}
			if got != tt.wantErr {
				t.Fatalf("validateFormat(%q) = %q, want %q", tt.format, got, tt.wantErr)
			}
		})
	}
}

func TestOutputFlagRejectsNonJSONBeforeRun(t *testing.T) {
	tests := []struct {
		value    string
		wantErr  bool
		wantJSON bool
	}{
		{value: "json", wantJSON: true},
		{value: ""},
		{value: "yaml", wantErr: true},
		{value: "JSON", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			ran := false
			cmd := &cobra.Command{Use: "x", SilenceUsage: true, SilenceErrors: true, RunE: func(*cobra.Command, []string) error {
				ran = true
				return nil
			}}
			AddOutputFlag(cmd)
			cmd.SetArgs([]string{"-o", tt.value})
			err := cmd.Execute()
			if (err != nil) != tt.wantErr || ran == tt.wantErr {
				t.Fatalf("-o %q: err = %v, ran = %t; want rejected before run = %t", tt.value, err, ran, tt.wantErr)
			}
			if got := WantJSON(cmd); got != tt.wantJSON {
				t.Errorf("-o %q: WantJSON = %t, want %t", tt.value, got, tt.wantJSON)
			}
		})
	}
}
