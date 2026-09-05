package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestCHNICOpsLiveNICsPlacesEveryNet(t *testing.T) {
	hc, _ := newCHStubClient(t, []chNet{
		{ID: "_net0", MAC: "aa:bb:cc:dd:ee:00", TAP: "tapvm1beef-0"},
		{ID: "cocoon-net-aabbccddee01", MAC: "aa:bb:cc:dd:ee:01", TAP: "tapvm1beef-1"},
		{ID: "_net9", MAC: "aa:bb:cc:dd:ee:99", TAP: "restore-tap"},
	})
	live, err := chNICOps{hc: hc}.LiveNICs(t.Context())
	if err != nil {
		t.Fatalf("LiveNICs: %v", err)
	}
	want := []hypervisor.LiveNIC{{ID: "_net0", Index: 0}, {ID: "cocoon-net-aabbccddee01", Index: 1}, {ID: "_net9", Index: -1}}
	if !slices.Equal(live, want) {
		t.Fatalf("live = %+v, want boot and hot-added NICs placed by TAP slot and the unplaceable one at -1", live)
	}
}

func TestCHNICOpsRemoveNICWaitsForEject(t *testing.T) {
	hc, removed := newCHStubClient(t, []chNet{
		{ID: "cocoon-net-aabbccddee02", MAC: "aa:bb:cc:dd:ee:02", TAP: "tapvm1beef-1"},
	}, "cocoon-net-aabbccddee02")
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := chNICOps{hc: hc}.RemoveNIC(ctx, "cocoon-net-aabbccddee02")
	if !errors.Is(err, hypervisor.ErrEjectPending) {
		t.Fatalf("err = %v, want ErrEjectPending for a device the guest never ejects", err)
	}
	if got := removed(); len(got) != 1 || got[0] != "cocoon-net-aabbccddee02" {
		t.Fatalf("removed = %v, want vm.remove-device issued before the wait", got)
	}
}

func TestConvergeOrphanedPause(t *testing.T) {
	var (
		mu      sync.Mutex
		resumes int
		state   = chStatePaused
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.resume", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		resumes++
		state = "Running"
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{State: state})
	})
	hc := newStubHTTPClient(t, mux)

	info, err := getVMInfo(t.Context(), hc)
	if err != nil {
		t.Fatalf("vm.info: %v", err)
	}
	fresh, err := convergeOrphanedPause(t.Context(), hc, "vm1", info)
	if err != nil {
		t.Fatalf("convergeOrphanedPause: %v", err)
	}
	if resumes != 1 {
		t.Fatalf("resumes = %d, want the orphaned pause resumed once", resumes)
	}

	if fresh.State != "Running" {
		t.Errorf("returned state = %q, want the refreshed Running", fresh.State)
	}
	if _, err = convergeOrphanedPause(t.Context(), hc, "vm1", fresh); err != nil {
		t.Fatalf("convergeOrphanedPause on a running VM: %v", err)
	}
	if resumes != 1 {
		t.Errorf("resumes = %d, want a running VM left untouched", resumes)
	}
}

func newStubHTTPClient(t *testing.T, mux *http.ServeMux) *http.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	return &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}}
}

func newCHStubClient(t *testing.T, nets []chNet, stickyIDs ...string) (*http.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var removed []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		removed = append(removed, req.ID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		live := make([]chNet, 0, len(nets))
		tree := map[string]json.RawMessage{}
		for _, n := range nets {
			if slices.Contains(removed, n.ID) {
				if slices.Contains(stickyIDs, n.ID) {
					tree[n.ID] = json.RawMessage("{}")
				}
				continue
			}
			live = append(live, n)
			tree[n.ID] = json.RawMessage("{}")
		}
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{Config: chVMInfoConfig{Nets: live}, DeviceTree: tree})
	})
	hc := newStubHTTPClient(t, mux)
	return hc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(removed)
	}
}
