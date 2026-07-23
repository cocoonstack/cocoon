package core

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/firecracker"
	"github.com/cocoonstack/cocoon/images"
	"github.com/cocoonstack/cocoon/images/cloudimg"
	"github.com/cocoonstack/cocoon/images/oci"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/metering/metalog"
	"github.com/cocoonstack/cocoon/network/cni"
	"github.com/cocoonstack/cocoon/snapshot/localfile"
)

const keyTombstones = "tombstones"

var (
	metaOnce  sync.Once
	metaStore meta.Store
	metaErr   error

	// vmTables maps the legacy vms.json fields onto the vms-namespace tables;
	// the json field names are engine knowledge and live only here.
	vmTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "vms", Table: hypervisor.TableRecords},
		{Key: "names", Table: hypervisor.TableNames},
		{Key: "orphan_dirs", Table: hypervisor.TableOrphanDirs, Optional: true, StringList: true},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
	snapTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "snapshots", Table: localfile.TableRecords},
		{Key: "names", Table: localfile.TableNames},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
	netTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "networks", Table: cni.TableRecords},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
	imageTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "images", Table: images.TableRecords},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
)

// MetaNamespaces lists every namespace with its tables — the engine-neutral
// declaration both engines consume.
func MetaNamespaces() []metasqlite.Namespace {
	return []metasqlite.Namespace{
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor)), Tables: []string{hypervisor.TableRecords, hypervisor.TableNames, hypervisor.TableOrphanDirs, tombstone.TableName}},
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorFirecracker)), Tables: []string{hypervisor.TableRecords, hypervisor.TableNames, hypervisor.TableOrphanDirs, tombstone.TableName}},
		{Name: localfile.NamespaceName, Tables: []string{localfile.TableRecords, localfile.TableNames, tombstone.TableName}},
		{Name: oci.NamespaceName, Tables: []string{images.TableRecords, tombstone.TableName}},
		{Name: cloudimg.NamespaceName, Tables: []string{images.TableRecords, tombstone.TableName}},
		{Name: cni.NamespaceName, Tables: []string{cni.TableRecords, tombstone.TableName}},
		{Name: metalog.NamespaceName, Tables: []string{metalog.TableEntries}},
	}
}

// MetaJSONNamespaces declares the json engine's namespace set: legacy file
// locations and field mappings, consumed by open and by conversion.
func MetaJSONNamespaces(conf *config.Config) []metajson.Namespace {
	chCfg := cloudhypervisor.NewConfig(conf)
	fcCfg := firecracker.NewConfig(conf)
	snapCfg := localfile.NewConfig(conf)
	cniCfg := cni.NewConfig(conf)
	ociCfg := oci.NewConfig(conf.RootDir, 0)
	cloudimgCfg := cloudimg.NewConfig(conf.RootDir, 0)
	return []metajson.Namespace{
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor)), FilePath: chCfg.IndexFile(), LockPath: chCfg.IndexLock(), Codec: vmTables},
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorFirecracker)), FilePath: fcCfg.IndexFile(), LockPath: fcCfg.IndexLock(), Codec: vmTables},
		{Name: localfile.NamespaceName, FilePath: snapCfg.IndexFile(), LockPath: snapCfg.IndexLock(), Codec: snapTables},
		{Name: oci.NamespaceName, FilePath: ociCfg.IndexFile(), LockPath: ociCfg.IndexLock(), Codec: imageTables},
		{Name: cloudimg.NamespaceName, FilePath: cloudimgCfg.IndexFile(), LockPath: cloudimgCfg.IndexLock(), Codec: imageTables},
		{Name: cni.NamespaceName, FilePath: cniCfg.IndexFile(), LockPath: cniCfg.IndexLock(), Codec: netTables},
		{
			Name:     metalog.NamespaceName,
			FilePath: filepath.Join(conf.RootDir, meteringSubdir, "log.json"),
			LockPath: filepath.Join(conf.RootDir, meteringSubdir, "log.lock"),
			Codec:    metajson.GenericCodec{},
		},
	}
}

// MetaDBPath is the sqlite engine's database under the meta root.
func MetaDBPath(conf *config.Config) string {
	return filepath.Join(conf.RootDir, "meta", metasqlite.DBFileName)
}

// MetaStore builds the process-wide meta store once — one store, every
// namespace — and injects it into every backend (design §10 P0 boundary).
// The engine follows conf.MetaBackend: json (default) or sqlite.
func MetaStore(conf *config.Config) (meta.Store, error) {
	metaOnce.Do(func() {
		// Ordinary opens of EITHER engine refuse while a conversion is in
		// flight (§6); the json engine cannot see the manifest itself.
		if err := metasqlite.RefuseManifest(MetaDBPath(conf)); err != nil {
			metaErr = err
			return
		}
		// Assign the interface only on success: a typed-nil store would pass
		// CloseMetaStore's nil check and panic.
		if conf.MetaBackend == "sqlite" {
			if s, err := metasqlite.Open(MetaDBPath(conf), MetaNamespaces()...); err != nil {
				metaErr = err
			} else {
				metaStore = s
			}
			return
		}
		if s, err := metajson.Open(MetaJSONNamespaces(conf)...); err != nil {
			metaErr = err
		} else {
			metaStore = s
		}
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
