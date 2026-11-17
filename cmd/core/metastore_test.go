package core

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/cmd/meta/convert"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/meta"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/utils"
)

func TestMetaStoreOpenErrorThenCloseNoPanic(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	conf.MetaBackend = "sqlite"
	writeLegacyJSON(t, conf)
	if _, err := MetaStore(conf); err == nil {
		t.Fatal("want open error on explicit sqlite over an uninitialized legacy root")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestMetaStoreBootstrapsFreshRoot(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	s, err := MetaStore(conf)
	if err != nil {
		t.Fatalf("bootstrap fresh root: %v", err)
	}
	if s == nil {
		t.Fatal("nil store")
	}
	if !utils.FileExists(MetaDBPath(conf)) {
		t.Fatal("meta.db not created")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestMetaStoreJSONFreshRootLocksPeerNamespace(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	conf.MetaBackend = config.MetaBackendJSON
	s, err := MetaStore(conf)
	if err != nil {
		t.Fatalf("open fresh json root: %v", err)
	}
	ch := hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor))
	fc := hypervisor.VMNamespaceName(string(config.HypervisorFirecracker))
	if err := s.Update(t.Context(), meta.Scope{Write: ch, Read: []string{fc}}, meta.CommitDurable, func(meta.Writer) error { return nil }); err != nil {
		t.Fatalf("placement-shaped update on a fresh json root: %v", err)
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestMetaStoreKeepsLegacyJSON(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	writeLegacyJSON(t, conf)
	s, err := MetaStore(conf)
	if err != nil {
		t.Fatalf("open legacy root: %v", err)
	}
	if s == nil {
		t.Fatal("nil store")
	}
	if utils.FileExists(MetaDBPath(conf)) {
		t.Fatal("legacy root must not grow a sqlite store")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestMetaStoreRepairsCrashedBootstrap(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	if err := os.MkdirAll(filepath.Dir(MetaDBPath(conf)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetaDBPath(conf), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := MetaStore(conf)
	if err != nil {
		t.Fatalf("repair crashed bootstrap: %v", err)
	}
	if s == nil {
		t.Fatal("nil store")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestMetaStoreRefusesJSONOverSQLiteData(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	if _, err := MetaStore(conf); err != nil {
		t.Fatalf("bootstrap sqlite root: %v", err)
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
	conf.MetaBackend = config.MetaBackendJSON
	if _, err := MetaStore(conf); err == nil {
		t.Fatal("json backend opened over a live meta.db and would hide its records")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestConvertRefusesWhileMetaStoreOpen(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	writeLegacyJSON(t, conf)
	if _, err := MetaStore(conf); err != nil {
		t.Fatalf("open legacy root: %v", err)
	}
	dbPath := MetaDBPath(conf)
	spec := convert.Spec{
		MetaRoot:   filepath.Dir(dbPath),
		DBPath:     dbPath,
		DaemonLock: filepath.Join(conf.RootDir, "locks", "daemon.lock"),
		Decls:      MetaNamespaces(),
		JSON:       MetaJSONNamespaces(conf),
	}
	err := convert.Run(t.Context(), spec, config.MetaBackendSQLite)
	if err == nil || !strings.Contains(err.Error(), "hold the meta store") {
		t.Fatalf("want refusal while a store handle is open, got %v", err)
	}
	if !utils.FileExists(MetaJSONNamespaces(conf)[0].FilePath) || utils.FileExists(dbPath) {
		t.Fatal("the refused convert touched the stores")
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
	if err := convert.Run(t.Context(), spec, config.MetaBackendSQLite); err != nil {
		t.Fatalf("convert after the store closed: %v", err)
	}
}

func TestMetaStoreBlamesConvertOnlyForATimeout(t *testing.T) {
	resetMetaStore()
	conf := testConf(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(MetaDBPath(conf)), metasqlite.StoreLockName), 0o750); err != nil {
		t.Fatalf("block the store lock path: %v", err)
	}
	_, err := MetaStore(conf)
	if err == nil || strings.Contains(err.Error(), "meta convert") {
		t.Fatalf("MetaStore = %v, want the lock open failure itself", err)
	}
	CloseMetaStore(t.Context())
	resetMetaStore()
}

func TestResolveMetaBackend(t *testing.T) {
	conf := testConf(t)
	if b := ResolveMetaBackend(conf); b != "sqlite" {
		t.Fatalf("fresh root: want sqlite, got %q", b)
	}
	writeLegacyJSON(t, conf)
	if b := ResolveMetaBackend(conf); b != "json" {
		t.Fatalf("legacy root: want json, got %q", b)
	}
	if err := os.MkdirAll(filepath.Dir(MetaDBPath(conf)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetaDBPath(conf), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := ResolveMetaBackend(conf); b != "sqlite" {
		t.Fatalf("meta.db present: want sqlite, got %q", b)
	}
	conf.MetaBackend = "json"
	if b := ResolveMetaBackend(conf); b != "json" {
		t.Fatalf("explicit setting must win, got %q", b)
	}
}

func resetMetaStore() {
	metaOnce = sync.Once{}
	metaStore = nil
	metaLease = nil
	metaErr = nil
}

func testConf(t *testing.T) *config.Config {
	dir := t.TempDir()
	return &config.Config{RootDir: dir, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log")}
}

func writeLegacyJSON(t *testing.T, conf *config.Config) {
	p := MetaJSONNamespaces(conf)[0].FilePath
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"vms":{},"names":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}
