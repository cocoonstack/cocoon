package cloudimg

import (
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (c *CloudImg) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(images.GCModuleConfig[imageIndex]{
		Name:     typ,
		Locker:   c.locker,
		Store:    c.store,
		ReadRefs: func(idx *imageIndex) map[string]struct{} { return images.ReferencedDigests(idx.Images) },
		ScanDisk: func() ([]string, error) { return utils.ScanFileStems(c.conf.BlobsDir(), ".qcow2") },
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
