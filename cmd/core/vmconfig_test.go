package core

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRestoreModeFromFlags(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr bool
	}{
		{name: "default empty", mode: "", want: ""},
		{name: "copy", mode: "copy", want: "copy"},
		{name: "ondemand", mode: "ondemand", want: "ondemand"},
		{name: "mmap", mode: "mmap", want: "mmap"},
		{name: "invalid", mode: "lazy", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("restore-mode", "", "")
			if tt.mode != "" {
				if err := cmd.Flags().Set("restore-mode", tt.mode); err != nil {
					t.Fatalf("set flag: %v", err)
				}
			}
			got, err := restoreModeFromFlags(cmd)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
