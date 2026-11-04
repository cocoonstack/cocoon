package oci

import (
	"os"

	"github.com/cocoonstack/cocoon/gc"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/utils"
)

func (o *OCI) GCModule() gc.Module[images.ImageGCSnapshot] {
	cfg := o.GCConfig()
	cfg.ExtraDisk = func() ([]string, error) { return utils.ScanSubdirs(o.conf.BootBaseDir()) }
	cfg.Removers = append(cfg.Removers, func(hex string) error { return os.RemoveAll(o.conf.BootDir(hex)) })
	return images.BuildGCModule(cfg)
}

func (o *OCI) RegisterGC(orch *gc.Orchestrator) {
	orch.Register(o.GCModule())
}
