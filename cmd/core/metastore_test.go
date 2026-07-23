package core

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/utils"
)

func resetMetaStore() {
	metaOnce = sync.Once{}
	metaStore = nil
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

// A failed engine open must leave the interface nil so CloseMetaStore's nil
// check holds — a typed-nil store would panic at command teardown.
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
