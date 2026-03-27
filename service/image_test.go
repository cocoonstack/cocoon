package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/cocoon/progress"
	"github.com/projecteru2/cocoon/types"
)

func TestListImages_Success(t *testing.T) {
	imgs := defaultImages()
	imgs.ListFunc = func(_ context.Context) ([]*types.Image, error) {
		return []*types.Image{
			{ID: "img-1", Name: "ubuntu:24.04", Type: "oci", CreatedAt: time.Now()},
			{ID: "img-2", Name: "debian:12", Type: "oci", CreatedAt: time.Now()},
		}, nil
	}
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	images, err := svc.ListImages(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(images) != 2 {
		t.Errorf("expected 2 images, got %d", len(images))
	}
}

func TestListImages_Empty(t *testing.T) {
	imgs := defaultImages()
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	images, err := svc.ListImages(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(images) != 0 {
		t.Errorf("expected 0 images, got %d", len(images))
	}
}

func TestListImages_BackendError(t *testing.T) {
	imgs := defaultImages()
	imgs.ListFunc = func(_ context.Context) ([]*types.Image, error) {
		return nil, fmt.Errorf("storage error")
	}
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	_, err := svc.ListImages(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRemoveImages_Success(t *testing.T) {
	imgs := defaultImages()
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	deleted, err := svc.RemoveImages(context.Background(), []string{"img-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted, got %d", len(deleted))
	}
}

func TestInspectImage_Found(t *testing.T) {
	imgs := defaultImages()
	imgs.InspectFunc = func(_ context.Context, ref string) (*types.Image, error) {
		return &types.Image{ID: ref, Name: "ubuntu:24.04"}, nil
	}
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	img, err := svc.InspectImage(context.Background(), "img-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if img.ID != "img-1" {
		t.Errorf("expected img-1, got %s", img.ID)
	}
}

func TestInspectImage_NotFound(t *testing.T) {
	imgs := defaultImages()
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	_, err := svc.InspectImage(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for not found")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestPullImage_CloudimgBackendMissing(t *testing.T) {
	imgs := defaultImages()
	svc := newTestService(defaultHypervisor(), imgs, nil, nil)

	// URL ref → tries cloudimg backend, but mock is not *cloudimg.CloudImg
	err := svc.PullImage(context.Background(), "https://example.com/test.img", progress.Nop)
	if err == nil {
		t.Fatal("expected error for missing cloudimg backend")
	}

	if !strings.Contains(err.Error(), "no cloudimg backend") {
		t.Errorf("expected 'no cloudimg backend' in error, got: %v", err)
	}
}

func TestImportImage_NoFiles(t *testing.T) {
	svc := newTestService(defaultHypervisor(), defaultImages(), nil, nil)

	err := svc.ImportImage(context.Background(), "test", nil, nil)
	if err == nil {
		t.Fatal("expected error for empty files")
	}

	if !strings.Contains(err.Error(), "no files") {
		t.Errorf("expected 'no files' in error, got: %v", err)
	}
}
