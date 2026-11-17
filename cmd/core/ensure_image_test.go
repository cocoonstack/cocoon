package core

import (
	"context"
	"slices"
	"testing"

	imagebackend "github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/types"
)

func TestEnsureImage_ForceWhenDigestPinned(t *testing.T) {
	const (
		url    = "https://epoch.example/dl/simular/win11"
		digest = "sha256:adafd938488daa114be898848eb24b9b0afffc21ac18f8b11f3f0057644b11e1"
	)

	tests := []struct {
		name      string
		vmCfg     *types.VMConfig
		wantForce bool
	}{
		{
			name: "digest pinned -> force=true",
			vmCfg: &types.VMConfig{
				Image:       url,
				ImageDigest: digest,
				ImageType:   types.ImageTypeCloudImg,
			},
			wantForce: true,
		},
		{
			name: "no digest -> force=false",
			vmCfg: &types.VMConfig{
				Image:     url,
				ImageType: types.ImageTypeCloudImg,
			},
			wantForce: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeImageBackend{typ: types.ImageTypeCloudImg}
			EnsureImage(t.Context(), []imagebackend.Images{f}, tt.vmCfg)
			if len(f.pullRefs) != 1 {
				t.Fatalf("Pull invocations = %d, want 1", len(f.pullRefs))
			}
			if f.pullRefs[0] != url {
				t.Errorf("Pull ref = %q, want %q", f.pullRefs[0], url)
			}
			if f.pullForce[0] != tt.wantForce {
				t.Errorf("Pull force = %v, want %v", f.pullForce[0], tt.wantForce)
			}
		})
	}
}

func TestEnsureImage_PullsOnlyGoodShape(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		imageType string
		wantPull  bool
	}{
		{"cloudimg bare OCI ref", "simular/win10:22h2-20260510", types.ImageTypeCloudImg, false},
		{"cloudimg local name", "win11", types.ImageTypeCloudImg, false},
		{"cloudimg non-http scheme", "file:///foo.img", types.ImageTypeCloudImg, false},
		{"oci malformed ref", "::bad::", types.ImageTypeOCI, false},
		{"cloudimg https url", "https://cloud-images.ubuntu.com/x.img", types.ImageTypeCloudImg, true},
		{"oci tagged ref", "ghcr.io/cocoonstack/cocoon/ubuntu:24.04", types.ImageTypeOCI, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeImageBackend{typ: tt.imageType}
			EnsureImage(t.Context(), []imagebackend.Images{f}, &types.VMConfig{Image: tt.image, ImageType: tt.imageType})
			var want []string
			if tt.wantPull {
				want = []string{tt.image}
			}
			if !slices.Equal(f.pullRefs, want) {
				t.Errorf("Pull = %v, want %v", f.pullRefs, want)
			}
		})
	}
}

func TestEnsureImage_SkipsPullWhenDigestLocal(t *testing.T) {
	const digest = "sha256:adafd938488daa114be898848eb24b9b0afffc21ac18f8b11f3f0057644b11e1"
	f := &fakeImageBackend{
		typ: types.ImageTypeCloudImg,
		inspectByRef: map[string]*types.Image{
			digest: {ID: digest, Name: "win11", Type: types.ImageTypeCloudImg},
		},
	}
	EnsureImage(t.Context(), []imagebackend.Images{f}, &types.VMConfig{
		Image:       "https://epoch.example/dl/simular/win11",
		ImageDigest: digest,
		ImageType:   types.ImageTypeCloudImg,
	})
	if len(f.pullRefs) != 0 {
		t.Errorf("Pull called %d time(s), want 0", len(f.pullRefs))
	}
}

type fakeImageBackend struct {
	imagebackend.Images
	typ          string
	inspectByRef map[string]*types.Image
	pullRefs     []string
	pullForce    []bool
}

func (f *fakeImageBackend) Type() string { return f.typ }

func (f *fakeImageBackend) Pull(_ context.Context, ref string, force bool, _ progress.Tracker) error {
	f.pullRefs = append(f.pullRefs, ref)
	f.pullForce = append(f.pullForce, force)
	return nil
}

func (f *fakeImageBackend) Inspect(_ context.Context, ref string) (*types.Image, error) {
	return f.inspectByRef[ref], nil
}
