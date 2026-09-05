package core

import (
	"context"
	"testing"

	"github.com/cocoonstack/cocoon/gc"
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
		name        string
		vmCfg       *types.VMConfig
		wantPullRef string
		wantForce   bool
	}{
		{
			name: "digest pinned -> force=true",
			vmCfg: &types.VMConfig{
				Config: types.Config{
					Image:       url,
					ImageDigest: digest,
					ImageType:   types.ImageTypeCloudImg,
				},
			},
			wantPullRef: url,
			wantForce:   true,
		},
		{
			name: "no digest -> force=false",
			vmCfg: &types.VMConfig{
				Config: types.Config{
					Image:     url,
					ImageType: types.ImageTypeCloudImg,
				},
			},
			wantPullRef: url,
			wantForce:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeImageBackend{typ: types.ImageTypeCloudImg}
			EnsureImage(t.Context(), []imagebackend.Images{f}, tt.vmCfg)
			if len(f.pullRefs) != 1 {
				t.Fatalf("Pull invocations = %d, want 1", len(f.pullRefs))
			}
			if f.pullRefs[0] != tt.wantPullRef {
				t.Errorf("Pull ref = %q, want %q", f.pullRefs[0], tt.wantPullRef)
			}
			if f.pullForce[0] != tt.wantForce {
				t.Errorf("Pull force = %v, want %v", f.pullForce[0], tt.wantForce)
			}
		})
	}
}

func TestEnsureImage_SkipsBadShape(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		imageType string
	}{
		{"cloudimg bare OCI ref", "simular/win10:22h2-20260510", types.ImageTypeCloudImg},
		{"cloudimg local name", "win11", types.ImageTypeCloudImg},
		{"cloudimg non-http scheme", "file:///foo.img", types.ImageTypeCloudImg},
		{"oci malformed ref", "::bad::", types.ImageTypeOCI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeImageBackend{typ: tt.imageType}
			EnsureImage(t.Context(), []imagebackend.Images{f}, &types.VMConfig{
				Config: types.Config{Image: tt.image, ImageType: tt.imageType},
			})
			if len(f.pullRefs) != 0 {
				t.Errorf("Pull called %d time(s) with %v, want 0 (shape should have failed)", len(f.pullRefs), f.pullRefs)
			}
		})
	}
}

func TestEnsureImage_AcceptsGoodShape(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		imageType string
	}{
		{"cloudimg https url", "https://cloud-images.ubuntu.com/x.img", types.ImageTypeCloudImg},
		{"oci tagged ref", "ghcr.io/cocoonstack/cocoon/ubuntu:24.04", types.ImageTypeOCI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeImageBackend{typ: tt.imageType}
			EnsureImage(t.Context(), []imagebackend.Images{f}, &types.VMConfig{
				Config: types.Config{Image: tt.image, ImageType: tt.imageType},
			})
			if len(f.pullRefs) != 1 || f.pullRefs[0] != tt.image {
				t.Errorf("Pull = %v, want one call for %q", f.pullRefs, tt.image)
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
		Config: types.Config{
			Image:       "https://epoch.example/dl/simular/win11",
			ImageDigest: digest,
			ImageType:   types.ImageTypeCloudImg,
		},
	})
	if len(f.pullRefs) != 0 {
		t.Errorf("Pull called %d time(s), want 0", len(f.pullRefs))
	}
}

type fakeImageBackend struct {
	typ          string
	inspectByRef map[string]*types.Image
	pullRefs     []string
	pullForce    []bool
}

func (f *fakeImageBackend) Type() string { return f.typ }

func (f *fakeImageBackend) PinBlobs(context.Context, map[string]struct{}) (func(), error) {
	return func() {}, nil
}

func (f *fakeImageBackend) Pull(_ context.Context, ref string, force bool, _ progress.Tracker) error {
	f.pullRefs = append(f.pullRefs, ref)
	f.pullForce = append(f.pullForce, force)
	return nil
}

func (f *fakeImageBackend) Inspect(_ context.Context, ref string) (*types.Image, error) {
	return f.inspectByRef[ref], nil
}

func (f *fakeImageBackend) Import(context.Context, string, progress.Tracker, ...string) error {
	return nil
}

func (f *fakeImageBackend) List(context.Context) ([]*types.Image, error) { return nil, nil }
func (f *fakeImageBackend) Delete(context.Context, []string) ([]string, error) {
	return nil, nil
}
func (f *fakeImageBackend) RegisterGC(*gc.Orchestrator) {}
func (f *fakeImageBackend) Config(context.Context, []*types.VMConfig) ([][]*types.StorageConfig, []*types.BootConfig, error) {
	return nil, nil, nil
}
