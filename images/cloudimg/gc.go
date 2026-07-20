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
		Store:    c.store,
		LockPath: c.conf.BlobLockPath,
		ReadRefs: images.ReferencedDigests[imageEntry],
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(c.conf.BlobsDir(), c.conf.BlobExt) },
		Removers: []func(string) error{
			func(hex string) error { return os.Remove(c.conf.BlobPath(hex)) },
		},
		TempDir: c.conf.TempDir(),
		DirOnly: false,
	})
}

func (c *CloudImg) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, c.GCModule())
}
