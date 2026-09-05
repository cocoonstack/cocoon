package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestFCNICOpsRoundTrip(t *testing.T) {
	srv := newFCStub(t, fcVMConfig{NetworkInterfaces: []fcNetworkInterface{{IfaceID: "eth0", HostDevName: "tap-0", GuestMAC: "aa:bb:cc:dd:ee:00"}}})
	ops := fcNICOps{hc: srv.client}
	ctx := t.Context()

	id, err := ops.AddNIC(ctx, 1, &types.NetworkConfig{TAP: "tap-1", MAC: "aa:bb:cc:dd:ee:01", MTU: 9000})
	if err != nil || id != "eth1" {
		t.Fatalf("AddNIC = %q, %v; want eth1", id, err)
	}
	live, err := ops.LiveNICs(ctx)
	if err != nil {
		t.Fatalf("LiveNICs: %v", err)
	}
	if len(live) != 2 || live[1] != (hypervisor.LiveNIC{ID: "eth1", Index: 1}) {
		t.Fatalf("live = %+v, want the hot-plugged eth1 placed at slot 1", live)
	}
	if got := srv.put("/network-interfaces/eth1"); !strings.Contains(got, `"mtu":9000`) {
		t.Fatalf("PUT body %s, want the TAP MTU advertised", got)
	}
	if err := ops.RemoveNIC(ctx, "eth1"); err != nil {
		t.Fatalf("RemoveNIC: %v", err)
	}
	if !slices.Equal(srv.deleted(), []string{"/network-interfaces/eth1"}) {
		t.Fatalf("deleted = %v, want eth1 unplugged", srv.deleted())
	}
}

func TestRequirePCI(t *testing.T) {
	rec := &hypervisor.VMRecord{VM: types.VM{ID: "vm1"}}
	if err := requirePCI(rec, disk.ErrUnsupportedBackend); err == nil || !strings.Contains(err.Error(), "MMIO") {
		t.Fatalf("err = %v, want an MMIO rejection wrapping the backend error", err)
	}
	rec.Config.PCI = true
	if err := requirePCI(rec, disk.ErrUnsupportedBackend); err != nil {
		t.Fatalf("a --pci VM must pass: %v", err)
	}
}

func TestCheckDriveFree(t *testing.T) {
	cfg := &fcVMConfig{Drives: []fcDrive{
		{DriveID: "drive_0", PathOnHost: "/layer.erofs"},
		{DriveID: hotDiskIDPrefix + "db", PathOnHost: "/vols/db.raw"},
	}}
	tests := []struct {
		name, id, path, wantErr string
	}{
		{"free", hotDiskIDPrefix + "cache", "/vols/cache.raw", ""},
		{"same name", hotDiskIDPrefix + "db", "/vols/other.raw", "already attached"},
		{"same path", hotDiskIDPrefix + "db2", "/vols/db.raw", "already attached as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDriveFree(cfg, tt.id, tt.path)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestHotDiskID(t *testing.T) {
	id, err := hotDiskID("scratch_1")
	if err != nil || id != "cocoon_disk_scratch_1" || hotDiskName(id) != "scratch_1" {
		t.Fatalf("hotDiskID = %q, %v; round trip %q", id, err, hotDiskName(id))
	}
	if _, err := hotDiskID("my-disk"); err == nil {
		t.Fatal("a hyphenated name must be rejected: Firecracker ids allow only [A-Za-z0-9_]")
	}
	if hotDiskName("drive_3") != "" || hotDiskName("cocoon_disk_") != "" {
		t.Fatal("create-path and malformed ids must not read as hot-attached disks")
	}
}

func TestRefuseHotAttachedDisks(t *testing.T) {
	srv := newFCStub(t, fcVMConfig{Drives: []fcDrive{{DriveID: "drive_0"}, {DriveID: hotDiskIDPrefix + "scratch", PathOnHost: "/vols/s.raw"}}})
	err := refuseHotAttachedDisks(t.Context(), srv.client)
	if err == nil || !strings.Contains(err.Error(), `"scratch"`) {
		t.Fatalf("err = %v, want the hot-attached disk named", err)
	}
	if got := hotAttachedDisks(&fcVMConfig{Drives: []fcDrive{{DriveID: "drive_1"}}}); got != nil {
		t.Fatalf("create-path drives listed as hot-attached: %+v", got)
	}
}

func TestConvergeOrphanedPause(t *testing.T) {
	srv := newFCStub(t, fcVMConfig{})
	srv.setState(vmStatePaused)
	if err := convergeOrphanedPause(t.Context(), srv.client, "vm1"); err != nil {
		t.Fatalf("convergeOrphanedPause: %v", err)
	}
	if srv.resumes() != 1 {
		t.Fatalf("resumes = %d, want the orphaned pause resumed once", srv.resumes())
	}
	if err := convergeOrphanedPause(t.Context(), srv.client, "vm1"); err != nil || srv.resumes() != 1 {
		t.Fatalf("running VM must be left alone: err=%v resumes=%d", err, srv.resumes())
	}
}

// fcStub fakes the Firecracker API: GET /, GET /vm/config, hot-plug PUT and DELETE on drives and network interfaces, PATCH /vm.
type fcStub struct {
	client *http.Client
	mu     sync.Mutex
	cfg    fcVMConfig
	state  string
	puts   map[string]string
	dels   []string
	resume int
}

func (s *fcStub) put(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[path]
}

func (s *fcStub) deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.dels)
}

func (s *fcStub) resumes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resume
}

func (s *fcStub) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

func (s *fcStub) handleDevice(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		s.puts[r.URL.Path] = string(body)
		if strings.HasPrefix(r.URL.Path, "/network-interfaces/") {
			var n fcNetworkInterface
			_ = json.Unmarshal(body, &n)
			s.cfg.NetworkInterfaces = append(s.cfg.NetworkInterfaces, n)
		} else {
			var d fcDrive
			_ = json.Unmarshal(body, &d)
			s.cfg.Drives = append(s.cfg.Drives, d)
		}
	case http.MethodDelete:
		s.dels = append(s.dels, r.URL.Path)
		id := path.Base(r.URL.Path)
		s.cfg.NetworkInterfaces = slices.DeleteFunc(s.cfg.NetworkInterfaces, func(n fcNetworkInterface) bool { return n.IfaceID == id })
		s.cfg.Drives = slices.DeleteFunc(s.cfg.Drives, func(d fcDrive) bool { return d.DriveID == id })
	default:
		http.Error(w, r.Method, http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newFCStub(t *testing.T, cfg fcVMConfig) *fcStub {
	t.Helper()
	s := &fcStub{cfg: cfg, state: "Running", puts: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(fcInstanceInfo{State: s.state})
	})
	mux.HandleFunc("/vm/config", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.cfg)
	})
	mux.HandleFunc("/vm", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["state"] == vmStateResumed {
			s.resume++
			s.state = "Running"
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/drives/", s.handleDevice)
	mux.HandleFunc("/network-interfaces/", s.handleDevice)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	s.client = &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", addr)
		},
	}}
	return s
}
