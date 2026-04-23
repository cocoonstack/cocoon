package oci

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/lock"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/storage"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	typ          = "oci"
	serialPrefix = "cocoon-layer"
)

var _ images.Images = (*OCI)(nil)

// OCI implements the images.Images interface using OCI container images
// converted to EROFS filesystems for use with Cloud Hypervisor.
type OCI struct {
	conf      *Config
	store     storage.Store[imageIndex]
	locker    lock.Locker
	pullGroup singleflight.Group
	ops       images.Ops[imageIndex, imageEntry]
}

// New creates a new OCI image backend.
func New(ctx context.Context, conf *config.Config) (*OCI, error) {
	if conf == nil {
		return nil, fmt.Errorf("config is nil")
	}
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	log.WithFunc("oci.New").Debugf(ctx, "OCI image backend initialized, pool size: %d", conf.PoolSize)

	store, locker := images.NewStore[imageIndex](cfg.IndexFile(), cfg.IndexLock())
	o := &OCI{
		conf:   cfg,
		store:  store,
		locker: locker,
		ops: images.Ops[imageIndex, imageEntry]{
			Store:      store,
			Type:       typ,
			LookupRefs: func(idx *imageIndex, q string) []string { return idx.LookupRefs(q) },
			Entries:    func(idx *imageIndex) map[string]*imageEntry { return idx.Images },
			Sizer:      imageSizer(cfg),
		},
	}
	return o, nil
}

// Type returns the image backend identifier.
func (o *OCI) Type() string { return typ }

// Pull downloads an OCI image from a container registry, extracts boot files
// (kernel, initrd), and converts each layer to EROFS concurrently.
func (o *OCI) Pull(ctx context.Context, image string, _ bool, tracker progress.Tracker) error {
	_, err, _ := o.pullGroup.Do(image, func() (any, error) {
		return nil, pull(ctx, o.conf, o.store, image, tracker)
	})
	return err
}

// Import imports local tar files as an OCI image.
// Each tar file becomes one EROFS layer (ordered by the files slice).
func (o *OCI) Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error {
	return importTarLayers(ctx, o.conf, o.store, name, tracker, file...)
}

// ImportFromReader imports a single tar layer from a reader (stdin, gzip stream, etc.).
func (o *OCI) ImportFromReader(ctx context.Context, name string, tracker progress.Tracker, r io.Reader) error {
	return importTarFromReader(ctx, o.conf, o.store, name, tracker, r)
}

// Inspect returns the record for a single image. Returns (nil, nil) if not found.
func (o *OCI) Inspect(ctx context.Context, id string) (*types.Image, error) {
	return o.ops.Inspect(ctx, id)
}

// List returns all locally stored images.
func (o *OCI) List(ctx context.Context) ([]*types.Image, error) {
	return o.ops.List(ctx)
}

// Delete removes images from the index.
// Returns the list of actually deleted refs. Images not found are logged and skipped.
func (o *OCI) Delete(ctx context.Context, ids []string) ([]string, error) {
	return o.ops.Delete(ctx, ids)
}

// Config generates StorageConfig and BootConfig entries for the given VMs.
// Paths are derived from layer digests at runtime, not stored in the index.
// Image references are normalized (e.g., "ubuntu:24.04" matches "docker.io/library/ubuntu:24.04").
// Returns an error if any referenced blob or boot file is missing on disk.
func (o *OCI) Config(ctx context.Context, vms []*types.VMConfig) (result [][]*types.StorageConfig, boot []*types.BootConfig, err error) {
	err = o.store.With(ctx, func(idx *imageIndex) error {
		result = make([][]*types.StorageConfig, len(vms))
		boot = make([]*types.BootConfig, len(vms))
		for i, vm := range vms {
			_, entry, ok := idx.Lookup(vm.Image)
			if !ok {
				return fmt.Errorf("image %q not found for VM %s", vm.Image, vm.Name)
			}
			vm.ImageDigest = entry.EntryID()
			vm.ImageType = o.Type()

			var configs []*types.StorageConfig
			for j, layer := range entry.Layers {
				blobPath := o.conf.BlobPath(layer.Digest.Hex())
				if !utils.ValidFile(blobPath) {
					return fmt.Errorf("blob invalid for VM %s layer %d (%s)", vm.Name, j, layer.Digest)
				}
				configs = append(configs, &types.StorageConfig{
					Path:   blobPath,
					RO:     true,
					Serial: fmt.Sprintf("%s%d", serialPrefix, j),
				})
			}
			result[i] = configs

			kernelPath := o.conf.KernelPath(entry.KernelLayer.Hex())
			initrdPath := o.conf.InitrdPath(entry.InitrdLayer.Hex())
			if !utils.ValidFile(kernelPath) {
				return fmt.Errorf("kernel invalid for VM %s (%s)", vm.Name, entry.KernelLayer)
			}
			if !utils.ValidFile(initrdPath) {
				return fmt.Errorf("initrd invalid for VM %s (%s)", vm.Name, entry.InitrdLayer)
			}
			boot[i] = &types.BootConfig{
				KernelPath: kernelPath,
				InitrdPath: initrdPath,
			}
		}
		return nil
	})
	return result, boot, err
}

func imageSizer(paths *Config) func(*imageEntry) int64 {
	return func(e *imageEntry) int64 {
		if e.Size > 0 {
			return e.Size
		}
		// Fallback for index entries created before Size was cached.
		var total int64
		for _, layer := range e.Layers {
			if info, err := os.Stat(paths.BlobPath(layer.Digest.Hex())); err == nil {
				total += info.Size()
			}
		}
		if e.KernelLayer != "" {
			if info, err := os.Stat(paths.KernelPath(e.KernelLayer.Hex())); err == nil {
				total += info.Size()
			}
		}
		if e.InitrdLayer != "" {
			if info, err := os.Stat(paths.InitrdPath(e.InitrdLayer.Hex())); err == nil {
				total += info.Size()
			}
		}
		return total
	}
}
