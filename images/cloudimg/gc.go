package cloudimg

import (
	"context"
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (c *CloudImg) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(images.GCModuleConfig[imageEntry]{
		Name:     typ,
		Store:    c.store,
		LockPath: c.conf.BlobLockPath,
		ReadRefs: images.ReferencedDigests[imageEntry],
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(c.conf.BlobsDir(), c.conf.BlobExt) },
		Removers: []func(string) error{
			func(hex string) error { return os.Remove(c.conf.BlobPath(hex)) },
		},
		TempDir:         c.conf.TempDir(),
		PinnedElsewhere: c.pinnedElsewhere,
		DirOnly:         false,
	})
}

func (c *CloudImg) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, c.GCModule())
}

// SetPinnedElsewhere injects the cross-subsystem pin recheck used by GC.
func (c *CloudImg) SetPinnedElsewhere(fn func(context.Context) (map[string]struct{}, error)) {
	c.pinnedElsewhere = fn
}

// PinBlobs implements images.Images: digest locks held while the caller commits a pin.
func (c *CloudImg) PinBlobs(_ context.Context, blobIDs map[string]struct{}) (func(), error) {
	return images.PinBlobs(&c.conf.BaseConfig, blobIDs)
}

// OwnsBlob reports whether this backend holds hex's blob file.
func (c *CloudImg) OwnsBlob(hex string) bool { return c.conf.OwnsBlob(hex) }
