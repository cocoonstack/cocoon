package core

import (
	"context"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/network/cni"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
)

var (
	metaOnce  sync.Once
	metaStore *metajson.Store
	metaErr   error
)

// MetaStore builds the process-wide meta store once — one store, every
// namespace — and injects it into every backend (design §10 P0 boundary).
func MetaStore(conf *config.Config) (*metajson.Store, error) {
	metaOnce.Do(func() {
		metaStore, metaErr = metajson.Open(
			cloudhypervisor.MetaNamespace(conf),
			firecracker.MetaNamespace(conf),
			localfile.MetaNamespace(conf),
			oci.MetaNamespace(conf.RootDir),
			cloudimg.MetaNamespace(conf.RootDir),
			cni.MetaNamespace(conf),
		)
	})
	return metaStore, metaErr
}

// CloseMetaStore ends the store's unified lifecycle at command teardown
// (design §10 P0); a process that never opened it is a no-op.
func CloseMetaStore(ctx context.Context) {
	if metaStore == nil {
		return
	}
	if err := metaStore.Close(); err != nil {
		log.WithFunc("core.CloseMetaStore").Warnf(ctx, "close meta store: %v", err)
	}
}
