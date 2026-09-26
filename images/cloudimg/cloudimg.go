// Package cloudimg implements the cloud image backend for UEFI boot.
package cloudimg

import (
	"context"
	"fmt"
	"io"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"

	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/progress"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const typ = types.ImageTypeCloudImg

var _ images.Images = (*CloudImg)(nil)

// CloudImg stores cloud image blobs for UEFI boot under Cloud Hypervisor.
type CloudImg struct {
	images.Ops[imageEntry]

	conf            *Config
	store           *images.Store[imageEntry]
	pullGroup       singleflight.Group
	pinnedElsewhere images.PinRecheck
}

// New builds the cloud image backend under rootDir; pullConns <= 0 defaults to 8 concurrent Range connections.
func New(ctx context.Context, rootDir string, pullConns int, metaStore meta.Store) (*CloudImg, error) {
	cfg := NewConfig(rootDir, pullConns)
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	log.WithFunc("cloudimg.New").Debugf(ctx, "cloud image backend initialized, pull conns: %d", cfg.PullConns)

	store := images.NewMetaStore[imageEntry](metaStore, NamespaceName)
	c := &CloudImg{
		conf:  cfg,
		store: store,
		Ops: images.Ops[imageEntry]{
			Store: store,
			Type:  typ,
		},
	}
	return c, nil
}

func (c *CloudImg) Type() string { return typ }

func (c *CloudImg) Pull(ctx context.Context, url string, force bool, tracker progress.Tracker) error {
	key := url
	if force {
		// A forced refresh must not dedup onto an in-flight non-force pull — it would return success without ever refreshing the cached blob.
		key += "\x00force"
	}
	return images.SingleflightDo(ctx, &c.pullGroup, key, func() error {
		return pull(ctx, c.conf, c.store, url, force, tracker)
	})
}

func (c *CloudImg) Import(ctx context.Context, name string, tracker progress.Tracker, file ...string) error {
	if len(file) == 1 {
		return importQcow2File(ctx, c.conf, c.store, name, tracker, file[0])
	}
	return importQcow2Concat(ctx, c.conf, c.store, name, tracker, file...)
}

func (c *CloudImg) ImportFromReader(ctx context.Context, name string, tracker progress.Tracker, r io.Reader) error {
	return importQcow2Reader(ctx, c.conf, c.store, name, tracker, r)
}

func (c *CloudImg) Config(ctx context.Context, vm *types.VMConfig) (configs []*types.StorageConfig, boot *types.BootConfig, err error) {
	err = c.store.View(ctx, func(idx *imageIndex) error {
		firmwarePath := images.FirmwarePath(c.conf.RootDir)
		if !utils.ValidFile(firmwarePath) {
			return fmt.Errorf("firmware not found: %s", firmwarePath)
		}
		_, entry, ok := images.LookupOne(idx.Images, vm.Image)
		if !ok {
			return fmt.Errorf("image %q not found for VM %s", vm.Image, vm.Name)
		}
		blobPath := c.conf.BlobPath(entry.ContentSum.Hex())
		if !utils.ValidFile(blobPath) {
			return fmt.Errorf("blob invalid for VM %s (%s)", vm.Name, entry.ContentSum)
		}
		// Stamped last: ResolveImage probes every backend, and a loser must not leave its identity on the VM.
		vm.ImageDigest = entry.EntryID()
		vm.ImageType = c.Type()

		configs = []*types.StorageConfig{{
			Path:   blobPath,
			RO:     true,
			Serial: "cocoon-base",
			Role:   types.StorageRoleLayer,
		}}
		boot = &types.BootConfig{FirmwarePath: firmwarePath}
		return nil
	})
	return configs, boot, err
}
