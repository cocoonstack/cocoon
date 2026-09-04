package core

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"time"

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
	"github.com/cocoonstack/cocoon/utils"
)

const (
	keyTombstones = "tombstones"

	metaBootstrapTimeout = 10 * time.Second
)

var (
	metaOnce  sync.Once
	metaStore meta.Store
	metaErr   error

	// vmTables maps the legacy vms.json fields onto the vms-namespace tables; the json field names are engine knowledge and live only here.
	vmTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "vms", Table: hypervisor.TableRecords},
		{Key: "names", Table: hypervisor.TableNames},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
	snapTables = metajson.TableCodec{Specs: []metajson.TableSpec{
		{Key: "snapshots", Table: localfile.TableRecords},
		{Key: "names", Table: localfile.TableNames},
		{Key: keyTombstones, Table: tombstone.TableName, Optional: true},
	}}
)

// MetaNamespaces lists every namespace with its tables — the engine-neutral declaration both engines consume.
func MetaNamespaces() []metasqlite.Namespace {
	return []metasqlite.Namespace{
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor)), Tables: []string{hypervisor.TableRecords, hypervisor.TableNames, tombstone.TableName}},
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorFirecracker)), Tables: []string{hypervisor.TableRecords, hypervisor.TableNames, tombstone.TableName}},
		{Name: localfile.NamespaceName, Tables: []string{localfile.TableRecords, localfile.TableNames, tombstone.TableName}},
		{Name: oci.NamespaceName, Tables: []string{images.TableRecords, tombstone.TableName}},
		{Name: cloudimg.NamespaceName, Tables: []string{images.TableRecords, tombstone.TableName}},
		{Name: cni.NamespaceName, Tables: []string{cni.TableRecords, tombstone.TableName}},
		{Name: metalog.NamespaceName, Tables: []string{metalog.TableEntries}},
	}
}

// MetaJSONNamespaces declares the json engine's namespace set: legacy file locations and field mappings, consumed by open and by conversion.
func MetaJSONNamespaces(conf *config.Config) []metajson.Namespace {
	chCfg := cloudhypervisor.NewConfig(conf)
	fcCfg := firecracker.NewConfig(conf)
	snapCfg := localfile.NewConfig(conf)
	return []metajson.Namespace{
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor)), FilePath: chCfg.IndexFile(), LockPath: chCfg.IndexLock(), Codec: vmTables},
		{Name: hypervisor.VMNamespaceName(string(config.HypervisorFirecracker)), FilePath: fcCfg.IndexFile(), LockPath: fcCfg.IndexLock(), Codec: vmTables},
		{Name: localfile.NamespaceName, FilePath: snapCfg.IndexFile(), LockPath: snapCfg.IndexLock(), Codec: snapTables},
		ImageJSONNamespace(&oci.NewConfig(conf.RootDir, 0).BaseConfig),
		ImageJSONNamespace(&cloudimg.NewConfig(conf.RootDir, 0).BaseConfig),
		cni.NewConfig(conf).JSONNamespace(),
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

// ResolveMetaBackend returns the effective engine: an explicit setting wins, then an existing store binds (meta.db → sqlite, legacy json files → json), and a fresh root gets sqlite.
func ResolveMetaBackend(conf *config.Config) string {
	if conf.MetaBackend != "" {
		return conf.MetaBackend
	}
	if utils.FileExists(MetaDBPath(conf)) {
		return config.MetaBackendSQLite
	}
	if LegacyJSONPresent(conf) {
		return config.MetaBackendJSON
	}
	return config.MetaBackendSQLite
}

// LegacyJSONPresent reports whether any json-engine namespace file exists under the root — data a fresh sqlite store must never shadow.
func LegacyJSONPresent(conf *config.Config) bool {
	return slices.ContainsFunc(MetaJSONNamespaces(conf), func(ns metajson.Namespace) bool {
		return utils.FileExists(ns.FilePath)
	})
}

// MetaStore builds the process-wide meta store once — one store, every namespace — and injects it into every backend (design §10 P0 boundary). The engine follows ResolveMetaBackend; a fresh sqlite root bootstraps itself.
func MetaStore(conf *config.Config) (meta.Store, error) {
	metaOnce.Do(func() {
		// Bootstrap owns its context: the store outlives any single caller, and a canceled first caller must not poison the Once for everyone.
		ctx, cancel := context.WithTimeout(context.Background(), metaBootstrapTimeout)
		defer cancel()
		// Ordinary opens of EITHER engine refuse while a conversion is in flight (§6); the json engine cannot see the manifest itself.
		dbPath := MetaDBPath(conf)
		if err := metasqlite.RefuseManifest(dbPath); err != nil {
			metaErr = err
			return
		}
		// Assign the interface only on success: a typed-nil store would pass CloseMetaStore's nil check and panic.
		if ResolveMetaBackend(conf) == config.MetaBackendSQLite {
			if s, err := openSQLiteStore(ctx, conf, dbPath); err != nil {
				metaErr = err
			} else {
				metaStore = s
			}
			return
		}
		if conf.MetaBackend == "" {
			log.WithFunc("core.MetaStore").Info(ctx, "legacy json meta store in use; `cocoon meta convert` upgrades it to sqlite")
		}
		if s, err := metajson.Open(MetaJSONNamespaces(conf)...); err != nil {
			metaErr = err
		} else {
			metaStore = s
		}
	})
	return metaStore, metaErr
}

// CloseMetaStore ends the store's unified lifecycle at command teardown (design §10 P0); a process that never opened it is a no-op.
func CloseMetaStore(ctx context.Context) {
	if metaStore == nil {
		return
	}
	if err := metaStore.Close(); err != nil {
		log.WithFunc("core.CloseMetaStore").Warnf(ctx, "close meta store: %v", err)
	}
}

// ImageJSONNamespace describes an image store's json namespace from its base paths.
func ImageJSONNamespace(c *images.BaseConfig) metajson.Namespace {
	return metajson.Namespace{
		Name:     c.Name,
		FilePath: c.IndexFile(),
		LockPath: c.IndexLock(),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "images", Table: images.TableRecords},
			{Key: tombstone.TableName, Table: tombstone.TableName, Optional: true},
		}},
	}
}

// openSQLiteStore opens the sqlite engine, bootstrapping a fresh root or repairing a crashed bootstrap; a legacy json root never bootstraps — that would shadow its data.
func openSQLiteStore(ctx context.Context, conf *config.Config, dbPath string) (meta.Store, error) {
	if !LegacyJSONPresent(conf) {
		if err := metasqlite.InitIfMissing(ctx, dbPath, MetaNamespaces()...); err != nil {
			return nil, err
		}
	}
	s, err := metasqlite.Open(dbPath, MetaNamespaces()...)
	if err != nil {
		return nil, err
	}
	return s, nil
}
