package cloudimg

import (
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (c *CloudImg) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(images.GCModuleConfig[imageEntry]{
		Name:     typ,
		Store:    c.Store,
		LockPath: c.conf.BlobLockPath,
		ReadRefs: images.ReferencedDigests[imageEntry],
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(c.conf.BlobsDir(), c.conf.BlobExt) },
		Removers: []images.BlobRemover{
			func(hex string) error { return os.Remove(c.conf.BlobPath(hex)) },
		},
		TempDir:         c.conf.TempDir(),
		PinnedElsewhere: c.PinnedElsewhere,
	})
}

func (c *CloudImg) RegisterGC(orch *gc.Orchestrator) {
	orch.Register(c.GCModule())
}

func (c *CloudImg) BlobPath(hex string) string { return c.conf.BlobPath(hex) }
