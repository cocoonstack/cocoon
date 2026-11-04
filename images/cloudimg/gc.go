package cloudimg

import (
	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
)

func (c *CloudImg) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(c.GCConfig())
}

func (c *CloudImg) RegisterGC(orch *gc.Orchestrator) {
	orch.Register(c.GCModule())
}

func (c *CloudImg) BlobPath(hex string) string { return c.conf.BlobPath(hex) }
