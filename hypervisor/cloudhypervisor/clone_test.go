package cloudhypervisor

import (
	"encoding/binary"
	"encoding/json/v2"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestPatchCHConfig_PreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeCHConfig(t, dir, baseCHConfig())

	if err := patchCHConfig(path, basePatchOpts()); err != nil {
		t.Fatalf("patchCHConfig: %v", err)
	}

	result := readRawJSON(t, path)

	platform, ok := result["platform"].(map[string]any)
	if !ok {
		t.Fatal("platform section lost")
	}
	if platform["uuid"] != "12345678-1234-1234-1234-123456789abc" {
		t.Errorf("platform.uuid lost: got %v", platform["uuid"])
	}

	cpus := result["cpus"].(map[string]any)
	if _, ok := cpus["max_phys_bits"]; !ok {
		t.Error("cpus.max_phys_bits lost")
	}

	mem := result["memory"].(map[string]any)
	if _, ok := mem["hotplug_method"]; !ok {
		t.Error("memory.hotplug_method lost")
	}
	if _, ok := mem["thp"]; !ok {
		t.Error("memory.thp lost")
	}

	disks := result["disks"].([]any)
	for i, d := range disks {
		disk := d.(map[string]any)
		if _, ok := disk["pci_segment"]; !ok {
			t.Errorf("disk[%d].pci_segment lost", i)
		}
		if _, ok := disk["disable_io_uring"]; !ok {
			t.Errorf("disk[%d].disable_io_uring lost", i)
		}
		if _, ok := disk["id"]; !ok {
			t.Errorf("disk[%d].id lost", i)
		}
	}

	nets := result["net"].([]any)
	net0 := nets[0].(map[string]any)
	if net0["id"] != "_net0" {
		t.Errorf("net[0].id lost: got %v", net0["id"])
	}
	if _, ok := net0["pci_segment"]; !ok {
		t.Error("net[0].pci_segment lost")
	}
}

func TestPatchCHConfig_UpdatesDiskPaths(t *testing.T) {
	dir := t.TempDir()
	path := writeCHConfig(t, dir, baseCHConfig())

	opts := basePatchOpts()
	if err := patchCHConfig(path, opts); err != nil {
		t.Fatal(err)
	}

	result := readRawJSON(t, path)
	disks := result["disks"].([]any)

	disk0 := disks[0].(map[string]any)
	if disk0["path"] != "/new/layer.erofs" {
		t.Errorf("disk[0].path: got %v, want /new/layer.erofs", disk0["path"])
	}

	disk1 := disks[1].(map[string]any)
	if disk1["path"] != "/new/cow.raw" {
		t.Errorf("disk[1].path: got %v, want /new/cow.raw", disk1["path"])
	}

	if disk0["id"] != "_disk0" {
		t.Errorf("disk[0].id changed: got %v", disk0["id"])
	}
}

func TestPatchCHConfig_SerialConsole(t *testing.T) {
	t.Run("direct_boot", func(t *testing.T) {
		dir := t.TempDir()
		path := writeCHConfig(t, dir, baseCHConfig())

		opts := basePatchOpts()
		opts.directBoot = true
		if err := patchCHConfig(path, opts); err != nil {
			t.Fatal(err)
		}

		result := readRawJSON(t, path)
		serial := result["serial"].(map[string]any)
		if serial["mode"] != "Off" {
			t.Errorf("serial.mode: got %v, want Off", serial["mode"])
		}
		console := result["console"].(map[string]any)
		if console["mode"] != "Pty" {
			t.Errorf("console.mode: got %v, want Pty", console["mode"])
		}
	})

	t.Run("uefi_boot", func(t *testing.T) {
		dir := t.TempDir()
		cfg := baseCHConfig()
		cfg["payload"] = map[string]any{"firmware": "/boot/OVMF.fd"}
		path := writeCHConfig(t, dir, cfg)

		opts := basePatchOpts()
		opts.directBoot = false
		opts.consoleSock = "/new/console.sock"
		if err := patchCHConfig(path, opts); err != nil {
			t.Fatal(err)
		}

		result := readRawJSON(t, path)
		serial := result["serial"].(map[string]any)
		if serial["mode"] != "Socket" {
			t.Errorf("serial.mode: got %v, want Socket", serial["mode"])
		}
		if serial["socket"] != "/new/console.sock" {
			t.Errorf("serial.socket: got %v", serial["socket"])
		}
		console := result["console"].(map[string]any)
		if console["mode"] != "Off" {
			t.Errorf("console.mode: got %v, want Off", console["mode"])
		}
	})
}

func TestPatchCHConfig_DiskCountMismatch(t *testing.T) {
	dir := t.TempDir()
	path := writeCHConfig(t, dir, baseCHConfig())

	opts := basePatchOpts()

	opts.storageConfigs = append(opts.storageConfigs, &types.StorageConfig{Path: "/extra"})
	err := patchCHConfig(path, opts)
	if err == nil {
		t.Fatal("expected error for disk count mismatch")
	}
	if !strings.Contains(err.Error(), "array length mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPatchCHConfig_RetargetsNetTAPs(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCHConfig()
	cfg["net"] = []any{
		map[string]any{"id": "_net2", "tap": "btSOURCE00-0", "mac": "96:39:e0:7e:03:6d", "num_queues": 2},
		map[string]any{"id": "_net3", "tap": "btSOURCE00-1", "mac": "96:39:e0:7e:03:6e", "num_queues": 2},
	}
	path := writeCHConfig(t, dir, cfg)

	opts := basePatchOpts()
	opts.netTAPs = []string{"btCLONE000-0", "rmCLONE000-1"}
	if err := patchCHConfig(path, opts); err != nil {
		t.Fatalf("patchCHConfig: %v", err)
	}

	result := readRawJSON(t, path)
	nets := result["net"].([]any)
	for i, want := range opts.netTAPs {
		n := nets[i].(map[string]any)
		if n["tap"] != want {
			t.Errorf("net[%d].tap: got %v, want %v", i, n["tap"], want)
		}
	}
	n0 := nets[0].(map[string]any)
	if n0["id"] != "_net2" {
		t.Errorf("net[0].id changed: got %v", n0["id"])
	}
	if n0["mac"] != "96:39:e0:7e:03:6d" {
		t.Errorf("net[0].mac changed: got %v", n0["mac"])
	}
}

func TestPatchCHConfig_QueueAffinity(t *testing.T) {
	tests := []struct {
		name      string
		queueCPUs []int
		want      []chQueueAffinity
	}{
		{name: "no pins drop the source host's mapping", queueCPUs: nil},
		{name: "pins re-derive the mapping", queueCPUs: []int{8, 9}, want: []chQueueAffinity{
			{QueueIndex: 0, HostCPUs: []int{8}},
			{QueueIndex: 1, HostCPUs: []int{9}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := baseCHConfig()
			sourceAffinity := []any{
				map[string]any{"queue_index": 0, "host_cpus": []any{0}},
				map[string]any{"queue_index": 1, "host_cpus": []any{1}},
			}
			cfg["disks"].([]any)[0].(map[string]any)["queue_affinity"] = sourceAffinity
			cfg["disks"].([]any)[1].(map[string]any)["queue_affinity"] = sourceAffinity
			path := writeCHConfig(t, dir, cfg)

			opts := basePatchOpts()
			opts.cpu = 2
			opts.queueCPUs = tt.queueCPUs
			if err := patchCHConfig(path, opts); err != nil {
				t.Fatalf("patchCHConfig: %v", err)
			}

			patched, err := parseCHConfig(path)
			if err != nil {
				t.Fatalf("parseCHConfig: %v", err)
			}
			if got := patched.Disks[1].QueueAffinity; !slices.EqualFunc(got, tt.want, sameQueueAffinity) {
				t.Errorf("cow disk: got %+v, want %+v", got, tt.want)
			}
			if got := patched.Disks[0].QueueAffinity; got != nil {
				t.Errorf("read-only layer: got %+v, want none", got)
			}
		})
	}
}

func TestUpdateCOWPath_OCI(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/cow.raw", RO: false, Serial: hypervisor.CowSerial, Role: types.StorageRoleCOW},
	}
	if err := updateCOWPath(configs, "/new/cow.raw"); err != nil {
		t.Fatal(err)
	}
	if configs[0].Path != "/old/layer.erofs" {
		t.Errorf("RO layer path changed: %s", configs[0].Path)
	}
	if configs[1].Path != "/new/cow.raw" {
		t.Errorf("COW path not updated: %s", configs[1].Path)
	}
}

func TestUpdateCOWPath_NoCOW(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/data.raw", RO: false, Serial: "data1", Role: types.StorageRoleData},
	}
	err := updateCOWPath(configs, "/new/cow.raw")
	if err == nil {
		t.Fatal("expected error when no COW role disk")
	}
}

func TestCloneBlobIDsDirectBoot(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/blobs/layer0.erofs", Role: types.StorageRoleLayer},
		{Path: "/blobs/layer1.erofs", Role: types.StorageRoleLayer},
		{Path: "/run/vm/cow.raw", Role: types.StorageRoleCOW},
	}
	boot := &types.BootConfig{KernelPath: "/boot/layer1/vmlinuz", InitrdPath: "/boot/layer0/initrd.img"}
	got, err := cloneBlobIDs(configs, boot, "/run/vm/cow.raw")
	if err != nil {
		t.Fatalf("cloneBlobIDs: %v", err)
	}
	want := map[string]struct{}{"layer0": {}, "layer1": {}}
	if !maps.Equal(got, want) {
		t.Fatalf("blob IDs = %v, want %v", got, want)
	}
}

func TestCloneBlobIDsCloudimgPinsTheBackingBlob(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	writeQcow2(t, overlay, "/blobs/basehex.qcow2")
	configs := []*types.StorageConfig{{Path: overlay, Role: types.StorageRoleCOW}}
	boot := &types.BootConfig{FirmwarePath: "/firmware/CLOUDHV.fd"}
	got, err := cloneBlobIDs(configs, boot, overlay)
	if err != nil {
		t.Fatalf("cloneBlobIDs: %v", err)
	}
	want := map[string]struct{}{"basehex": {}}
	if !maps.Equal(got, want) {
		t.Fatalf("blob IDs = %v, want the backing blob %v and not the overlay", got, want)
	}
}

func TestCloneBlobIDsCloudimgWithoutBackingKeepsTheReservation(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	writeQcow2(t, overlay, "")
	got, err := cloneBlobIDs(nil, &types.BootConfig{FirmwarePath: "/firmware/CLOUDHV.fd"}, overlay)
	if err != nil {
		t.Fatalf("cloneBlobIDs: %v", err)
	}
	if got != nil {
		t.Fatalf("blob IDs = %v, want nil so FinalizeClone keeps the reserved set", got)
	}
}

func TestRestorePatchStorageConfigs(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/l", Role: types.StorageRoleLayer},
		{Path: "/d", Role: types.StorageRoleData},
		{Path: "/c", Role: types.StorageRoleCidata},
	}
	all := []string{"/l", "/d", "/c"}
	tests := []struct {
		name                           string
		directBoot, windows, hadCidata bool
		want                           []string
	}{
		{"drops appended cidata", false, false, false, []string{"/l", "/d"}},
		{"keeps all on direct boot", true, false, false, all},
		{"keeps all on windows", false, true, false, all},
		{"keeps all when the snapshot had cidata", false, false, true, all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := restorePatchStorageConfigs(configs, tt.directBoot, tt.windows, tt.hadCidata)
			paths := make([]string, len(got))
			for i, sc := range got {
				paths[i] = sc.Path
			}
			if !slices.Equal(paths, tt.want) {
				t.Errorf("got %v, want %v", paths, tt.want)
			}
		})
	}
}

func TestRestoreAndResumeCloneHotplugsByRoleWithQueueCPUs(t *testing.T) {
	var (
		mu    sync.Mutex
		added []chDisk
	)
	sock := serveCHHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "vm.add-disk") {
			var d chDisk
			if err := json.UnmarshalRead(r.Body, &d); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			added = append(added, d)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	storageConfigs := []*types.StorageConfig{
		{Path: "/store/cow.raw", Role: types.StorageRoleCOW, Serial: "cocoon-cow"},
		{Path: "/run/cidata.img", RO: true, Role: types.StorageRoleCidata},
		{Path: "/run/extra.raw", Role: types.StorageRoleData, Serial: "extra"},
	}
	scopeParent := t.TempDir()
	if err := os.Mkdir(filepath.Join(scopeParent, "vm-T.scope"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ch := &CloudHypervisor{
		Backend: &hypervisor.Backend{Conf: tmpScopeConf{Config: NewConfig(&config.Config{}), parent: scopeParent}},
		conf:    NewConfig(&config.Config{}),
	}
	if err := ch.restoreAndResumeClone(t.Context(), 0, sock, t.TempDir(), &cloneResumeOpts{
		vmID:           "T",
		vmCfg:          &types.VMConfig{CPU: 2},
		storageConfigs: storageConfigs,
		dataDisks:      storageConfigs[2:],
		snapshotCfg:    &chVMConfig{},
		queueCPUs:      []int{8, 9},
	}); err != nil {
		t.Fatalf("restoreAndResumeClone: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantPaths := []string{"/run/cidata.img", "/run/extra.raw"}
	gotPaths := make([]string, len(added))
	for i, d := range added {
		gotPaths[i] = d.Path
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("vm.add-disk paths = %v, want %v", gotPaths, wantPaths)
	}
	if got := added[0].QueueAffinity; got != nil {
		t.Errorf("read-only cidata: got %+v, want none", got)
	}
	wantAffinity := []chQueueAffinity{{QueueIndex: 0, HostCPUs: []int{8}}, {QueueIndex: 1, HostCPUs: []int{9}}}
	if got := added[1].QueueAffinity; !slices.EqualFunc(got, wantAffinity, sameQueueAffinity) {
		t.Errorf("data disk: got %+v, want %+v", got, wantAffinity)
	}
}

func sameQueueAffinity(a, b chQueueAffinity) bool {
	return a.QueueIndex == b.QueueIndex && slices.Equal(a.HostCPUs, b.HostCPUs)
}

func writeCHConfig(t *testing.T, dir string, cfg map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRawJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func baseCHConfig() map[string]any {
	return map[string]any{
		"payload": map[string]any{
			"kernel":    "/boot/vmlinux",
			"initramfs": "/boot/initrd",
			"cmdline":   "console=hvc0 old-cmdline",
		},
		"cpus": map[string]any{
			"boot_vcpus":    2,
			"max_vcpus":     8,
			"topology":      nil,
			"max_phys_bits": 46,
		},
		"memory": map[string]any{
			"size":           int64(1 << 30),
			"hugepages":      false,
			"hotplug_method": "Acpi",
			"shared":         false,
			"thp":            true,
		},
		"disks": []any{
			map[string]any{
				"id":               "_disk0",
				"path":             "/old/layer.erofs",
				"readonly":         true,
				"serial":           "layer0",
				"pci_segment":      0,
				"disable_io_uring": false,
			},
			map[string]any{
				"id":               "_disk1",
				"path":             "/old/cow.raw",
				"readonly":         false,
				"serial":           "cocoon-cow",
				"pci_segment":      0,
				"disable_io_uring": false,
				"sparse":           true,
			},
		},
		"net": []any{
			map[string]any{
				"id":          "_net0",
				"tap":         "old-tap0",
				"mac":         "aa:bb:cc:dd:ee:f0",
				"num_queues":  4,
				"queue_size":  256,
				"ip":          nil,
				"mask":        nil,
				"pci_segment": 0,
			},
		},
		"balloon": map[string]any{
			"id":                  "_balloon0",
			"size":                int64(1<<30) / 4,
			"deflate_on_oom":      true,
			"free_page_reporting": true,
		},
		"serial": map[string]any{
			"mode":   "Socket",
			"socket": "/old/console.sock",
		},
		"console": map[string]any{
			"mode": "Off",
		},
		"rng": map[string]any{
			"src": "/dev/urandom",
		},
		"watchdog": true,
		"platform": map[string]any{
			"num_pci_segments": 1,
			"uuid":             "12345678-1234-1234-1234-123456789abc",
			"serial_number":    nil,
			"oem_strings":      nil,
		},
	}
}

func basePatchOpts() *patchOptions {
	return &patchOptions{
		storageConfigs: []*types.StorageConfig{
			{Path: "/new/layer.erofs", RO: true, Serial: "layer0"},
			{Path: "/new/cow.raw", RO: false, Serial: "cocoon-cow"},
		},
		consoleSock: "/new/console.sock",
		directBoot:  true,
	}
}

type tmpScopeConf struct {
	*Config
	parent string
}

func (c tmpScopeConf) CgroupParentDir() string { return c.parent }

func writeQcow2(t *testing.T, path, backing string) {
	t.Helper()
	hdr := make([]byte, 64)
	copy(hdr, utils.Qcow2Magic)
	binary.BigEndian.PutUint32(hdr[4:8], 3)
	if backing != "" {
		binary.BigEndian.PutUint64(hdr[8:16], uint64(len(hdr)))
		binary.BigEndian.PutUint32(hdr[16:20], uint32(len(backing))) //nolint:gosec // short test path
	}
	if err := os.WriteFile(path, append(hdr, backing...), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
