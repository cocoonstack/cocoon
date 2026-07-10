package cloudhypervisor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

// TestEjectOrphanNICs covers the interrupted-resize fault window: a CH device
// whose MAC the VM record does not know must be ejected before a retry adds
// alongside it; recorded and boot-time (_net*) devices must survive.
func TestEjectOrphanNICs(t *testing.T) {
	var mu sync.Mutex
	var removed []string
	nets := []chNet{
		{ID: "cocoon-net-aabbccddee01", MAC: "aa:bb:cc:dd:ee:01"},
		{ID: "cocoon-net-aabbccddee02", MAC: "aa:bb:cc:dd:ee:02"},
		{ID: "_net0", MAC: "aa:bb:cc:dd:ee:99"},
	}
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
				continue
			}
			live = append(live, n)
			tree[n.ID] = json.RawMessage("{}")
		}
		_ = json.NewEncoder(w).Encode(chVMInfoResponse{Config: chVMInfoConfig{Nets: live}, DeviceTree: tree})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}}

	info, err := getVMInfo(t.Context(), hc)
	if err != nil {
		t.Fatalf("vm.info: %v", err)
	}
	recorded := []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}} // case-insensitive match
	if err := ejectOrphanNICs(t.Context(), hc, info, recorded); err != nil {
		t.Fatalf("ejectOrphanNICs: %v", err)
	}
	if len(removed) != 1 || removed[0] != "cocoon-net-aabbccddee02" {
		t.Fatalf("removed = %v, want only the unrecorded cocoon NIC", removed)
	}
}
