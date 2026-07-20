package oci

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	typ          = types.ImageTypeOCI
	serialPrefix = "cocoon-layer"
)

var _ images.Images = (*OCI)(nil)

// OCI converts OCI container layers to EROFS for Cloud Hypervisor.
type OCI struct {
	conf            *Config
	store           *images.Store[imageEntry]
	pullGroup       singleflight.Group
	ops             images.Ops[imageEntry]
	pinnedElsewhere func(context.Context) (map[string]struct{}, error)
}

// New builds the OCI backend under rootDir; poolSize <= 0 means NumCPU.
func New(ctx context.Context, rootDir string, poolSize int, metaStore meta.Store) (*OCI, error) {
	cfg := NewConfig(rootDir, poolSize)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	log.WithFunc("oci.New").Debugf(ctx, "OCI image backend initialized, pool size: %d", cfg.PoolSize)

	store := images.NewMetaStore[imageEntry](metaStore, NamespaceName)
	o := &OCI{
		conf:  cfg,
		store: store,
		ops: images.Ops[imageEntry]{
			Store:      store,
			Type:       typ,
			LookupRefs: func(m map[string]*imageEntry, id string) []string { return images.LookupRefs(m, id, normalizeRef) },
			Sizer:      imageSizer(cfg),
		},
	}
	return o, nil
}

func (o *OCI) Type() string { return typ }

// Pull downloads an OCI image, extracts boot files, and converts each layer to EROFS concurrently.
func (o *OCI) Pull(ctx context.Context, image string, _ bool, tracker progress.Tracker) error {
	return images.SingleflightDo(ctx, &o.pullGroup, image, func() error {
		return pull(ctx, o.conf, o.store, image, tracker)
	})
}

// Import: each tar file becomes one EROFS layer in the order of the files slice.
func (o *OCI) Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error {
	return importTarLayers(ctx, o.conf, o.store, name, tracker, file...)
}

func (o *OCI) ImportFromReader(ctx context.Context, name string, tracker progress.Tracker, r io.Reader) error {
	return importTarFromReader(ctx, o.conf, o.store, name, tracker, r)
}

// Inspect returns (nil, nil) if not found.
func (o *OCI) Inspect(ctx context.Context, id string) (*types.Image, error) {
	return o.ops.Inspect(ctx, id)
}

func (o *OCI) List(ctx context.Context) ([]*types.Image, error) {
	return o.ops.List(ctx)
}

// Delete returns actually-deleted refs; not-found ids are logged and skipped.
func (o *OCI) Delete(ctx context.Context, ids []string) ([]string, error) {
	return o.ops.Delete(ctx, ids)
}

// Config builds StorageConfig + BootConfig from layer digests; errors if any blob is missing.
func (o *OCI) Config(ctx context.Context, vms []*types.VMConfig) (result [][]*types.StorageConfig, boot []*types.BootConfig, err error) {
	err = o.store.View(ctx, func(idx *imageIndex) error {
		result = make([][]*types.StorageConfig, len(vms))
		boot = make([]*types.BootConfig, len(vms))
		for i, vm := range vms {
			_, entry, ok := images.LookupOne(idx.Images, vm.Image, normalizeRef)
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
					Role:   types.StorageRoleLayer,
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
