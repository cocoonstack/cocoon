package core

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/config"
)

// A failed engine open must leave the interface nil so CloseMetaStore's nil
// check holds — a typed-nil store would panic at command teardown.
func TestMetaStoreOpenErrorThenCloseNoPanic(t *testing.T) {
	dir := t.TempDir()
	conf := &config.Config{RootDir: dir, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log"), MetaBackend: "sqlite"}
	if _, err := MetaStore(conf); err == nil {
		t.Fatal("want open error on uninitialized sqlite root")
	}
	CloseMetaStore(t.Context())
}
