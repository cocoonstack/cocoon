package vfio

import (
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "short BDF", in: "01:00.0", want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "full BDF", in: "0000:01:00.0", want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "uppercase BDF", in: "00:1F.7", want: SysfsPCIPrefix + "0000:00:1f.7"},
		{name: "sysfs path", in: SysfsPCIPrefix + "0000:01:00.0", want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "sysfs path with trailing slash", in: SysfsPCIPrefix + "0000:01:00.0/", want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "sysfs path uppercase BDF", in: SysfsPCIPrefix + "0000:0A:00.0", want: SysfsPCIPrefix + "0000:0a:00.0"},
		{name: "absolute non-sysfs path rejected", in: "/dev/null", wantErr: "must be under"},
		{name: "sysfs prefix with garbage suffix", in: SysfsPCIPrefix + "garbage", wantErr: "BDF suffix"},
		{name: "sysfs prefix with short BDF", in: SysfsPCIPrefix + "01:00.0", wantErr: "BDF suffix"},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "garbage", in: "not-a-bdf", wantErr: "invalid"},
		{name: "bad function digit", in: "01:00.8", wantErr: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizePath(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpecNormalizedPath(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		want    string
		wantErr string
	}{
		{name: "valid short BDF", spec: Spec{PCI: "01:00.0"}, want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "valid full BDF with id", spec: Spec{PCI: "0000:01:00.0", ID: "mygpu"}, want: SysfsPCIPrefix + "0000:01:00.0"},
		{name: "missing pci", spec: Spec{}, wantErr: "pci is required"},
		{name: "id with cocoon prefix", spec: Spec{PCI: "01:00.0", ID: "cocoon-x"}, wantErr: "id"},
		{name: "id with bad chars", spec: Spec{PCI: "01:00.0", ID: "bad id"}, wantErr: "id"},
		{name: "bad pci", spec: Spec{PCI: "junk"}, wantErr: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.spec.NormalizedPath()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
