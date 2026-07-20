package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
)

func newStore(t *testing.T, dir string, nss ...string) *Store {
	t.Helper()
	decls := make([]Namespace, 0, len(nss))
	for _, ns := range nss {
		decls = append(decls, Namespace{Name: ns, Tables: []string{"records", "names", "tombstones"}})
	}
	path := filepath.Join(dir, DBFileName)
	if !exists(path) {
		if err := Init(path, decls...); err != nil {
			t.Fatalf("init: %v", err)
		}
	}
	s, err := Open(path, decls...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return newStore(t, t.TempDir(), nss...)
	})
}

func TestContractForcedRetryEngine(t *testing.T) {
	contracttest.Run(t, func(t *testing.T, nss []string) meta.Store {
		return contracttest.ForcedRetry(newStore(t, t.TempDir(), nss...))
	})
}
