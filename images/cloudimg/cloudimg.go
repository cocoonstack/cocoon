package cloudimg

import (
	"context"
	"fmt"
	"io"

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

// compile-time interface check.
var (
	_ images.Images = (*CloudImg)(nil)
)

const (
	typ = "cloudimg"
)

// CloudImg stores cloud image blobs for UEFI boot under Cloud Hypervisor.
type CloudImg struct {
	conf      *Config
	store     storage.Store[imageIndex]
	locker    lock.Locker
	pullGroup singleflight.Group
	ops       images.Ops[imageIndex, imageEntry]
}

// New creates a new cloud image backend.
func New(ctx context.Context, conf *config.Config) (*CloudImg, error) {
	cfg := NewConfig(conf)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	log.WithFunc("cloudimg.New").Debug(ctx, "cloud image backend initialized")

	store, locker := images.NewStore[imageIndex](cfg.IndexFile(), cfg.IndexLock())
	c := &CloudImg{
		conf:   cfg,
		store:  store,
		locker: locker,
		ops: images.Ops[imageIndex, imageEntry]{
			Store:      store,
			Type:       typ,
			LookupRefs: func(idx *imageIndex, q string) []string { return idx.LookupRefs(q) },
			Entries:    func(idx *imageIndex) map[string]*imageEntry { return idx.Images },
			Sizer:      imageSizer,
		},
	}
	return c, nil
}

// Type returns the image backend identifier.
func (c *CloudImg) Type() string { return typ }

// Pull downloads a cloud image and stores it in the blob cache.
func (c *CloudImg) Pull(ctx context.Context, url string, force bool, tracker progress.Tracker) error {
	_, err, _ := c.pullGroup.Do(url, func() (any, error) {
		return nil, pull(ctx, c.conf, c.store, url, force, tracker)
	})
	return err
}

// Import imports local qcow2 file(s) as a cloud image.
func (c *CloudImg) Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error {
	if len(file) == 1 {
		return importQcow2File(ctx, c.conf, c.store, name, tracker, file[0])
	}
	return importQcow2Concat(ctx, c.conf, c.store, name, tracker, file...)
}

// ImportFromReader imports a qcow2 image from a reader (stdin, gzip stream, etc.).
func (c *CloudImg) ImportFromReader(ctx context.Context, name string, tracker progress.Tracker, r io.Reader) error {
	return importQcow2Reader(ctx, c.conf, c.store, name, tracker, r)
}

// Inspect returns the record for a single image. Returns (nil, nil) if not found.
func (c *CloudImg) Inspect(ctx context.Context, id string) (*types.Image, error) {
	return c.ops.Inspect(ctx, id)
}

// List returns all locally stored cloud images.
func (c *CloudImg) List(ctx context.Context) ([]*types.Image, error) {
	return c.ops.List(ctx)
}

// Delete removes images from the index.
func (c *CloudImg) Delete(ctx context.Context, ids []string) ([]string, error) {
	return c.ops.Delete(ctx, ids)
}

// Config resolves cloud images to qcow2 storage plus firmware boot config.
func (c *CloudImg) Config(ctx context.Context, vms []*types.VMConfig) (result [][]*types.StorageConfig, boot []*types.BootConfig, err error) {
	err = c.store.With(ctx, func(idx *imageIndex) error {
		result = make([][]*types.StorageConfig, len(vms))
		boot = make([]*types.BootConfig, len(vms))
		for i, vm := range vms {
			_, entry, ok := idx.Lookup(vm.Image)
			if !ok {
				return fmt.Errorf("image %q not found for VM %s", vm.Image, vm.Name)
			}
			vm.ImageDigest = entry.EntryID()
			vm.ImageType = c.Type()

			blobPath := c.conf.BlobPath(entry.ContentSum.Hex())
			if !utils.ValidFile(blobPath) {
				return fmt.Errorf("blob invalid for VM %s (%s)", vm.Name, entry.ContentSum)
			}

			result[i] = []*types.StorageConfig{{
				Path:   blobPath,
				RO:     true,
				Serial: "cocoon-base",
			}}

			firmwarePath := c.conf.FirmwarePath()
			if !utils.ValidFile(firmwarePath) {
				return fmt.Errorf("firmware not found: %s", firmwarePath)
			}
			boot[i] = &types.BootConfig{
				FirmwarePath: firmwarePath,
			}
		}
		return nil
	})
	return result, boot, err
}
