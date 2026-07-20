package core

import (
	"testing"

	"github.com/cocoonstack/cocoon/config"
)

// TestInitAllHypervisorsFirecrackerPinned guards the desktop-isolation contract:
// a Firecracker-pinned engine (the sandbox data plane, COCOON_USE_FIRECRACKER)
// must construct ONLY the Firecracker backend. Constructing the Cloud-Hypervisor
// backend would run its NewBackend migration against a co-located desktop's VM
// index under the same root dir. Unpinned (desktop) still gets every backend.
func TestInitAllHypervisorsFirecrackerPinned(t *testing.T) {
	t.Run("pinned returns only firecracker", func(t *testing.T) {
		conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir(), UseFirecracker: true}
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

	t.Run("unpinned returns every backend", func(t *testing.T) {
		conf := &config.Config{RootDir: t.TempDir(), RunDir: t.TempDir(), LogDir: t.TempDir()}
		hypers, err := InitAllHypervisors(t.Context(), conf)
		if err != nil {
			t.Fatalf("InitAllHypervisors: %v", err)
		}
		if len(hypers) != len(hypervisorFactories) {
			t.Fatalf("unpinned engine constructed %d backends, want %d", len(hypers), len(hypervisorFactories))
		}
	})
}
