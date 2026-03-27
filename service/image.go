package service

import (
	"context"
	"fmt"

	"github.com/projecteru2/cocoon/images/cloudimg"
	"github.com/projecteru2/cocoon/images/oci"
	"github.com/projecteru2/cocoon/progress"
	"github.com/projecteru2/cocoon/types"
)

// PullImage pulls an image from a registry (OCI) or URL (cloudimg).
// The tracker receives progress events for UI feedback.
func (s *Service) PullImage(ctx context.Context, ref string, tracker progress.Tracker) error {
	if tracker == nil {
		tracker = progress.Nop
	}

	if IsURL(ref) {
		store := s.findCloudimgBackend()
		if store == nil {
			return fmt.Errorf("no cloudimg backend available")
		}

		if err := store.Pull(ctx, ref, tracker); err != nil {
			return fmt.Errorf("pull %s: %w", ref, err)
		}

		return nil
	}

	store := s.findOCIBackend()
	if store == nil {
		return fmt.Errorf("no OCI backend available")
	}

	if err := store.Pull(ctx, ref, tracker); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}

	return nil
}

// ImportImage imports local files as an image, auto-detecting file type.
func (s *Service) ImportImage(ctx context.Context, name string, files []string, tracker progress.Tracker) error {
	if tracker == nil {
		tracker = progress.Nop
	}

	if len(files) == 0 {
		return fmt.Errorf("no files to import")
	}

	// Auto-detect file type from first file.
	isQcow2 := cloudimg.IsQcow2File(files[0])

	if !isQcow2 && !oci.IsTarFile(files[0]) {
		return fmt.Errorf("cannot detect file type for %s (expected qcow2 or tar)", files[0])
	}

	if isQcow2 {
		store := s.findCloudimgBackend()
		if store == nil {
			return fmt.Errorf("no cloudimg backend available")
		}

		if err := store.Import(ctx, name, tracker, files...); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}

		return nil
	}

	store := s.findOCIBackend()
	if store == nil {
		return fmt.Errorf("no OCI backend available")
	}

	if err := store.Import(ctx, name, tracker, files...); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}

	return nil
}

// ListImages returns all images across all backends.
func (s *Service) ListImages(ctx context.Context) ([]*types.Image, error) {
	var all []*types.Image

	for _, b := range s.images {
		imgs, err := b.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", b.Type(), err)
		}

		all = append(all, imgs...)
	}

	return all, nil
}

// RemoveImages deletes images by reference across all backends.
func (s *Service) RemoveImages(ctx context.Context, refs []string) ([]string, error) {
	var allDeleted []string

	for _, b := range s.images {
		deleted, err := b.Delete(ctx, refs)
		if err != nil {
			return allDeleted, fmt.Errorf("delete %s: %w", b.Type(), err)
		}

		allDeleted = append(allDeleted, deleted...)
	}

	return allDeleted, nil
}

// InspectImage finds an image by reference across all backends.
func (s *Service) InspectImage(ctx context.Context, ref string) (*types.Image, error) {
	for _, b := range s.images {
		img, err := b.Inspect(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", b.Type(), err)
		}

		if img != nil {
			return img, nil
		}
	}

	return nil, fmt.Errorf("image %q not found", ref)
}

// findOCIBackend returns the first OCI backend, or nil if none.
func (s *Service) findOCIBackend() *oci.OCI {
	for _, b := range s.images {
		if o, ok := b.(*oci.OCI); ok {
			return o
		}
	}

	return nil
}

// findCloudimgBackend returns the first cloudimg backend, or nil if none.
func (s *Service) findCloudimgBackend() *cloudimg.CloudImg {
	for _, b := range s.images {
		if c, ok := b.(*cloudimg.CloudImg); ok {
			return c
		}
	}

	return nil
}
