package oci

import (
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (o *OCI) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(images.GCModuleConfig[imageEntry]{
		Name:      typ,
		Store:     o.Store,
		LockPath:  o.conf.BlobLockPath,
		ReadRefs:  images.ReferencedDigests[imageEntry],
		ScanDisk:  func() ([]string, error) { return utils.ScanFileStems(o.conf.BlobsDir(), o.conf.BlobExt) },
		ExtraDisk: func() ([]string, error) { return utils.ScanSubdirs(o.conf.BootBaseDir()) },
		Removers: []images.BlobRemover{
			func(hex string) error { return os.Remove(o.conf.BlobPath(hex)) },
			func(hex string) error { return os.RemoveAll(o.conf.BootDir(hex)) },
		},
		TempDir:         o.conf.TempDir(),
		PinnedElsewhere: o.PinnedElsewhere,
	})
}

func (o *OCI) RegisterGC(orch *gc.Orchestrator) {
	orch.Register(o.GCModule())
}
