package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	"github.com/cocoonstack/cocoon/meta/contracttest"
	"github.com/cocoonstack/cocoon/utils"
)

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

func TestClosedStoreErrors(t *testing.T) {
	s := newStore(t, t.TempDir(), "ns")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.View(t.Context(), []string{"ns"}, func(meta.Reader) error { return nil }); err == nil {
		t.Fatal("View after Close returned nil")
	}
	if err := s.Update(t.Context(), meta.Scope{Write: "ns"}, meta.CommitDurable, func(meta.Writer) error { return nil }); err == nil {
		t.Fatal("Update after Close returned nil")
	}
}

func newStore(t *testing.T, dir string, nss ...string) *Store {
	t.Helper()
	decls := make([]Namespace, 0, len(nss))
	for _, ns := range nss {
		decls = append(decls, Namespace{Name: ns, Tables: []string{"records", "names", "tombstones"}})
	}
	path := filepath.Join(dir, DBFileName)
	if !utils.FileExists(path) {
		if err := Init(t.Context(), path, decls...); err != nil {
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
