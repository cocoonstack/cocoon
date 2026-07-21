package core

import (
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
)

func resetMetaStoreForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		CloseMetaStore(t.Context())
		metaOnce = sync.Once{}
		metaStore = nil
		metaErr = nil
	}
	reset()
	t.Cleanup(reset)
}

// TestInitAllHypervisorsFirecrackerPinned guards the desktop-isolation contract:
// selecting Firecracker alone keeps normal multi-backend behavior; the explicit
// pin is what restricts list/gc to Firecracker on a shared sandbox host.
func TestInitAllHypervisorsFirecrackerPinned(t *testing.T) {
	t.Run("pinned returns only firecracker", func(t *testing.T) {
		resetMetaStoreForTest(t)
		conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir(), UseFirecracker: true, PinHypervisor: true}
		hypers, err := InitAllHypervisors(t.Context(), conf)
		if err != nil {
			t.Fatalf("InitAllHypervisors: %v", err)
		}
		if len(hypers) != 1 || hypers[0].Type() != string(config.HypervisorFirecracker) {
			got := make([]string, len(hypers))
			for i, h := range hypers {
				got[i] = h.Type()
			}
			t.Fatalf("pinned engine constructed %v, want only [%s]", got, config.HypervisorFirecracker)
		}
	})

	t.Run("selected but unpinned returns every backend", func(t *testing.T) {
		resetMetaStoreForTest(t)
		conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir(), UseFirecracker: true}
		hypers, err := InitAllHypervisors(t.Context(), conf)
		if err != nil {
			t.Fatalf("InitAllHypervisors: %v", err)
		}
		if len(hypers) != len(hypervisorFactories) {
			t.Fatalf("unpinned engine constructed %d backends, want %d", len(hypers), len(hypervisorFactories))
		}
	})
}

func TestMetaNamespacesFirecrackerPinned(t *testing.T) {
	ch := hypervisor.VMNamespaceName(string(config.HypervisorCloudHypervisor))
	fc := hypervisor.VMNamespaceName(string(config.HypervisorFirecracker))
	wholeSQLite := map[string]bool{}
	for _, ns := range MetaNamespaces() {
		wholeSQLite[ns.Name] = true
	}
	for _, tt := range []struct {
		name        string
		useFC       bool
		pinned      bool
		wantRuntime map[string]bool
	}{
		{name: "firecracker pinned", useFC: true, pinned: true, wantRuntime: map[string]bool{fc: true}},
		{name: "cloud hypervisor pinned", pinned: true, wantRuntime: map[string]bool{ch: true}},
		{name: "selected but unpinned", useFC: true, wantRuntime: map[string]bool{ch: true, fc: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conf := &config.Config{RootDir: t.TempDir(), UseFirecracker: tt.useFC, PinHypervisor: tt.pinned}
			wholeJSON := map[string]bool{}
			for _, ns := range MetaJSONNamespaces(conf) {
				wholeJSON[ns.Name] = true
			}
			if !wholeSQLite[ch] || !wholeSQLite[fc] || !wholeJSON[ch] || !wholeJSON[fc] {
				t.Fatalf("whole-store scope changed: sqlite=%v json=%v", wholeSQLite, wholeJSON)
			}
			runtimeJSON := map[string]bool{}
			for _, ns := range runtimeJSONNamespaces(conf) {
				runtimeJSON[ns.Name] = true
			}
			if runtimeJSON[ch] != tt.wantRuntime[ch] || runtimeJSON[fc] != tt.wantRuntime[fc] {
				t.Errorf("runtime json scope: ch=%v fc=%v, want ch=%v fc=%v", runtimeJSON[ch], runtimeJSON[fc], tt.wantRuntime[ch], tt.wantRuntime[fc])
			}
		})
	}
}
