package core

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestSanitizeVMName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ubuntu:24.04", "cocoon-ubuntu-24.04"},
		{"ghcr.io/foo/ubuntu:24.04", "cocoon-foo-ubuntu-24.04"},
		{"localhost:5000/myimg:latest", "cocoon-myimg"},
		{"library/nginx:1.25", "cocoon-nginx-1.25"},
		// Digest refs: tag/digest should be stripped, only repo kept.
		{"ghcr.io/ns/img@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "cocoon-ns-img"},
		{"ubuntu@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "cocoon-ubuntu"},
		// No tag, no digest — "latest" is implicit and omitted.
		{"ghcr.io/org/repo", "cocoon-org-repo"},
		{"alpine", "cocoon-alpine"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeVMName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeVMName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeVMName_Truncation(t *testing.T) {
	// Build an image ref that would produce a name > 63 chars.
	long := "ghcr.io/" + strings.Repeat("a", 80) + ":latest"
	got := sanitizeVMName(long)
	if len(got) > 63 {
		t.Errorf("name too long (%d chars): %q", len(got), got)
	}
}

func TestParseDataDiskSpec(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(*testing.T, types.DataDiskSpec)
	}{
		{
			name:  "size only",
			input: "size=1G",
			check: func(t *testing.T, s types.DataDiskSpec) {
				if s.Size != 1<<30 {
					t.Errorf("size: got %d", s.Size)
				}
				if s.MountPointSet {
					t.Error("MountPointSet must be false when mount= absent")
				}
			},
		},
		{
			name:  "all fields",
			input: "size=2G,name=db,fstype=ext4,mount=/mnt/db,directio=on",
			check: func(t *testing.T, s types.DataDiskSpec) {
				if s.Name != "db" || s.MountPoint != "/mnt/db" || !s.MountPointSet {
					t.Errorf("got %+v", s)
				}
				if s.DirectIO == nil || !*s.DirectIO {
					t.Errorf("directio: got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "directio off",
			input: "size=20M,directio=off",
			check: func(t *testing.T, s types.DataDiskSpec) {
				if s.DirectIO == nil || *s.DirectIO {
					t.Errorf("directio: got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "directio auto leaves nil",
			input: "size=20M,directio=auto",
			check: func(t *testing.T, s types.DataDiskSpec) {
				if s.DirectIO != nil {
					t.Errorf("directio auto must be nil, got %+v", s.DirectIO)
				}
			},
		},
		{
			name:  "explicit empty mount",
			input: "size=20M,mount=",
			check: func(t *testing.T, s types.DataDiskSpec) {
				if s.MountPoint != "" || !s.MountPointSet {
					t.Errorf("expected MountPointSet=true MountPoint=\"\", got %+v", s)
				}
			},
		},
		{name: "missing size", input: "name=foo", wantErr: true},
		{name: "size below minimum", input: "size=8M", wantErr: true},
		{name: "reserved prefix", input: "size=20M,name=cocoon-foo", wantErr: true},
		{name: "invalid name", input: "size=20M,name=BadName", wantErr: true},
		{name: "name too long", input: "size=20M,name=this_name_is_way_too_long_for_virtio", wantErr: true},
		{name: "fstype xfs rejected", input: "size=20M,fstype=xfs", wantErr: true},
		{name: "directio invalid", input: "size=20M,directio=maybe", wantErr: true},
		{name: "unknown key", input: "size=20M,bogus=1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := parseDataDiskSpec(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && tt.check != nil {
				tt.check(t, spec)
			}
		})
	}
}

func TestNormalizeDataDiskSpecs(t *testing.T) {
	t.Run("auto names skip explicitly used", func(t *testing.T) {
		specs := []types.DataDiskSpec{
			{Size: 1 << 30},
			{Size: 1 << 30, Name: "data1"},
			{Size: 1 << 30},
		}
		if err := normalizeDataDiskSpecs(specs); err != nil {
			t.Fatal(err)
		}
		// First auto picks data2 (data1 taken by spec[1]).
		if specs[0].Name != "data2" {
			t.Errorf("specs[0].Name = %q, want data2", specs[0].Name)
		}
		if specs[2].Name != "data3" {
			t.Errorf("specs[2].Name = %q, want data3", specs[2].Name)
		}
	})

	t.Run("default fstype ext4 default mount /mnt/<name>", func(t *testing.T) {
		specs := []types.DataDiskSpec{{Size: 1 << 30, Name: "x"}}
		if err := normalizeDataDiskSpecs(specs); err != nil {
			t.Fatal(err)
		}
		if specs[0].FSType != "ext4" || specs[0].MountPoint != "/mnt/x" {
			t.Errorf("got %+v", specs[0])
		}
	})

	t.Run("fstype none requires empty mount", func(t *testing.T) {
		specs := []types.DataDiskSpec{
			{Size: 1 << 30, Name: "x", FSType: "none", MountPoint: "/mnt/x", MountPointSet: true},
		}
		if err := normalizeDataDiskSpecs(specs); err == nil {
			t.Error("expected error for fstype=none with mount set")
		}
	})

	t.Run("explicit empty mount kept", func(t *testing.T) {
		specs := []types.DataDiskSpec{
			{Size: 1 << 30, Name: "x", MountPointSet: true, MountPoint: ""},
		}
		if err := normalizeDataDiskSpecs(specs); err != nil {
			t.Fatal(err)
		}
		if specs[0].MountPoint != "" {
			t.Errorf("expected empty mount preserved, got %q", specs[0].MountPoint)
		}
	})

	t.Run("duplicate names rejected", func(t *testing.T) {
		specs := []types.DataDiskSpec{
			{Size: 1 << 30, Name: "dup"},
			{Size: 1 << 30, Name: "dup"},
		}
		if err := normalizeDataDiskSpecs(specs); err == nil {
			t.Error("expected duplicate-name error")
		}
	})
}
