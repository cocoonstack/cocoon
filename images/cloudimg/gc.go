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
func (c *CloudImg) SetPinnedElsewhere(fn images.PinRecheck) {
	c.pinnedElsewhere = fn
}

func (c *CloudImg) PinBlobs(_ context.Context, blobIDs map[string]struct{}) (func(), error) {
	return images.PinBlobs(&c.conf.BaseConfig, blobIDs)
}

func (c *CloudImg) OwnsBlob(hex string) bool { return c.conf.OwnsBlob(hex) }
