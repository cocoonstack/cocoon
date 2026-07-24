package oci

import (
	"context"
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (o *OCI) GCModule() gc.Module[images.ImageGCSnapshot] {
	return images.BuildGCModule(images.GCModuleConfig[imageEntry]{
		Name:      typ,
		Store:     o.store,
		LockPath:  o.conf.BlobLockPath,
		ReadRefs:  images.ReferencedDigests[imageEntry],
		ScanDisk:  func() ([]string, error) { return utils.ScanFileStems(o.conf.BlobsDir(), o.conf.BlobExt) },
		ExtraDisk: func() ([]string, error) { return utils.ScanSubdirs(o.conf.BootBaseDir()) },
		Removers: []func(string) error{
			func(hex string) error { return os.Remove(o.conf.BlobPath(hex)) },
			func(hex string) error { return os.RemoveAll(o.conf.BootDir(hex)) },
		},
		TempDir:         o.conf.TempDir(),
		DirOnly:         true,
		PinnedElsewhere: o.pinnedElsewhere,
	})
}

// SetPinnedElsewhere injects the cross-subsystem pin recheck used by GC.
func (o *OCI) SetPinnedElsewhere(fn func(context.Context) (map[string]struct{}, error)) {
	o.pinnedElsewhere = fn
}

func (o *OCI) PinBlobs(_ context.Context, blobIDs map[string]struct{}) (func(), error) {
	return images.PinBlobs(&o.conf.BaseConfig, blobIDs)
}

// OwnsBlob reports whether this backend holds hex's blob file.
func (o *OCI) OwnsBlob(hex string) bool { return o.conf.OwnsBlob(hex) }

func (o *OCI) RegisterGC(orch *gc.Orchestrator) {
	gc.Register(orch, o.GCModule())
}
