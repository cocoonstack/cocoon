package cloudhypervisor

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

func TestPatchCHConfig_PreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writeCHConfig(t, dir, baseCHConfig())

	if err := patchCHConfig(path, basePatchOpts()); err != nil {
		t.Fatalf("patchCHConfig: %v", err)
	}

	result := readRawJSON(t, path)

	// Top-level: platform must survive.
	platform, ok := result["platform"].(map[string]any)
	if !ok {
		t.Fatal("platform section lost")
	}
	if platform["uuid"] != "12345678-1234-1234-1234-123456789abc" {
		t.Errorf("platform.uuid lost: got %v", platform["uuid"])
	}

	// cpus: topology and max_phys_bits must survive.
	cpus := result["cpus"].(map[string]any)
	if _, ok := cpus["max_phys_bits"]; !ok {
		t.Error("cpus.max_phys_bits lost")
	}

	// memory: hotplug_method, shared, thp must survive.
	mem := result["memory"].(map[string]any)
	if _, ok := mem["hotplug_method"]; !ok {
		t.Error("memory.hotplug_method lost")
	}
	if _, ok := mem["thp"]; !ok {
		t.Error("memory.thp lost")
	}

	// disks: pci_segment, disable_io_uring, id must survive.
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

	// net: pci_segment, id must survive.
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

	// Original device IDs preserved.
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
	// 3 storage configs vs 2 disks in config.
	opts.storageConfigs = append(opts.storageConfigs, &types.StorageConfig{Path: "/extra"})
	err := patchCHConfig(path, opts)
	if err == nil {
		t.Fatal("expected error for disk count mismatch")
	}
	if !strings.Contains(err.Error(), "disk count mismatch") {
		t.Errorf("unexpected error: %v", err)
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

func TestUpdateCOWPath_Cloudimg(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/base.qcow2", RO: true, Role: types.StorageRoleLayer},
		{Path: "/old/overlay.qcow2", RO: false, Role: types.StorageRoleCOW},
	}
	if err := updateCOWPath(configs, "/new/overlay.qcow2"); err != nil {
		t.Fatal(err)
	}
	if configs[0].Path != "/old/base.qcow2" {
		t.Errorf("RO path changed: %s", configs[0].Path)
	}
	if configs[1].Path != "/new/overlay.qcow2" {
		t.Errorf("writable path not updated: %s", configs[1].Path)
	}
}

func TestRebuildBootConfig(t *testing.T) {
	t.Run("nil_payload", func(t *testing.T) {
		cfg := &chVMConfig{}
		if boot := rebuildBootConfig(cfg); boot != nil {
			t.Errorf("expected nil, got %+v", boot)
		}
	})

	t.Run("kernel_initramfs", func(t *testing.T) {
		cfg := &chVMConfig{Payload: &chPayload{
			Kernel: "/vmlinux", Initramfs: "/initrd", Cmdline: "console=hvc0",
		}}
		boot := rebuildBootConfig(cfg)
		if boot == nil {
			t.Fatal("expected non-nil")
		}
		if boot.KernelPath != "/vmlinux" {
			t.Errorf("KernelPath: %q", boot.KernelPath)
		}
		if boot.InitrdPath != "/initrd" {
			t.Errorf("InitrdPath: %q", boot.InitrdPath)
		}
		if boot.Cmdline != "console=hvc0" {
			t.Errorf("Cmdline: %q", boot.Cmdline)
		}
	})

	t.Run("firmware", func(t *testing.T) {
		cfg := &chVMConfig{Payload: &chPayload{Firmware: "/OVMF.fd"}}
		boot := rebuildBootConfig(cfg)
		if boot == nil {
			t.Fatal("expected non-nil")
		}
		if boot.FirmwarePath != "/OVMF.fd" {
			t.Errorf("FirmwarePath: %q", boot.FirmwarePath)
		}
	})

	t.Run("empty_payload", func(t *testing.T) {
		cfg := &chVMConfig{Payload: &chPayload{}}
		if boot := rebuildBootConfig(cfg); boot != nil {
			t.Errorf("expected nil for empty payload, got %+v", boot)
		}
	})
}

func TestPatchStateJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := `{"disk_path":"/old/vm1/cow.raw","other":"/old/vm1/layer.erofs","nested":{"path":"/old/vm1/cow.raw"}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	diskPathMap := map[string]string{
		"/old/vm1/cow.raw":     "/new/vm2/cow.raw",
		"/old/vm1/layer.erofs": "/new/vm2/layer.erofs",
	}
	if err := patchStateJSON(path, diskPathMap); err != nil {
		t.Fatal(err)
	}

	result := readRawJSON(t, path)
	// disk_path key: replaced.
	if result["disk_path"] != "/new/vm2/cow.raw" {
		t.Errorf("disk_path not replaced: %v", result["disk_path"])
	}
	// "other" key: NOT a disk path key, must be unchanged.
	if result["other"] != "/old/vm1/layer.erofs" {
		t.Errorf("non-disk-path key 'other' should not be changed: %v", result["other"])
	}
	// nested.path: replaced.
	nested := result["nested"].(map[string]any)
	if nested["path"] != "/new/vm2/cow.raw" {
		t.Errorf("nested path not replaced: %v", nested["path"])
	}
}

func TestPatchStateJSON_NonTargetKeysUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// MAC bytes and non-disk-path string values must not be modified.
	original := `{"disk_path":"/old/vm1/cow.raw","virtio_net":{"mac":[170,187,204,221,238,240],"queues":4},"socket":"/old/vm1/cow.raw"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	replacements := map[string]string{
		"/old/vm1/cow.raw": "/new/vm2/cow.raw",
	}
	if err := patchStateJSON(path, replacements); err != nil {
		t.Fatal(err)
	}

	result := readRawJSON(t, path)
	// disk_path: replaced.
	if result["disk_path"] != "/new/vm2/cow.raw" {
		t.Errorf("disk_path not replaced: %v", result["disk_path"])
	}
	// "socket" has same value but wrong key: must be unchanged.
	if result["socket"] != "/old/vm1/cow.raw" {
		t.Errorf("non-target key 'socket' should not be changed: %v", result["socket"])
	}
	// MAC array untouched.
	virtioNet := result["virtio_net"].(map[string]any)
	mac := virtioNet["mac"].([]any)
	if len(mac) != 6 || mac[0] != float64(170) || mac[5] != float64(240) {
		t.Errorf("MAC array should be untouched: %v", mac)
	}
}

func TestPatchStateJSON_PrefixCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// /old/vm1 must not match /old/vm10 — exact match only.
	original := `{"disk_path":"/old/vm10/cow.raw","path":"/old/vm1/cow.raw"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	replacements := map[string]string{
		"/old/vm1/cow.raw": "/new/vm2/cow.raw",
	}
	if err := patchStateJSON(path, replacements); err != nil {
		t.Fatal(err)
	}

	result := readRawJSON(t, path)
	// disk_path has /old/vm10/... — no match, must be unchanged.
	if result["disk_path"] != "/old/vm10/cow.raw" {
		t.Errorf("prefix collision: disk_path changed to %v", result["disk_path"])
	}
	// path has exact match, must be replaced.
	if result["path"] != "/new/vm2/cow.raw" {
		t.Errorf("path not replaced: %v", result["path"])
	}
}

func TestPatchStateJSON_NestedArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := `{"disks":[{"disk_path":"/old/a.raw","readonly":true},{"disk_path":"/old/b.raw"}]}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	replacements := map[string]string{
		"/old/a.raw": "/new/a.raw",
		"/old/b.raw": "/new/b.raw",
	}
	if err := patchStateJSON(path, replacements); err != nil {
		t.Fatal(err)
	}

	result := readRawJSON(t, path)
	disks := result["disks"].([]any)
	d0 := disks[0].(map[string]any)
	d1 := disks[1].(map[string]any)
	if d0["disk_path"] != "/new/a.raw" {
		t.Errorf("disk[0] not replaced: %v", d0["disk_path"])
	}
	if d1["disk_path"] != "/new/b.raw" {
		t.Errorf("disk[1] not replaced: %v", d1["disk_path"])
	}
}

func TestPatchStateJSON_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := patchStateJSON(path, map[string]string{"/old": "/new"})
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestBuildStateReplacements(t *testing.T) {
	chCfg := &chVMConfig{
		Disks: []chDisk{
			{Path: "/old/layer.erofs"},
			{Path: "/old/cow.raw"},
		},
	}
	storageConfigs := []*types.StorageConfig{
		{Path: "/old/layer.erofs"}, // unchanged
		{Path: "/new/cow.raw"},     // changed
	}

	m := buildStateReplacements(chCfg, storageConfigs)

	// Disk path: only changed one.
	if m["/old/cow.raw"] != "/new/cow.raw" {
		t.Errorf("disk replacement missing or wrong: %v", m)
	}
	if _, ok := m["/old/layer.erofs"]; ok {
		t.Error("unchanged disk path should not be in replacements")
	}
	if len(m) != 1 {
		t.Errorf("expected 1 replacement, got %d: %v", len(m), m)
	}
}

func TestBuildStateReplacements_PrefixMin(t *testing.T) {
	// ensureCloneCidata appends a fresh cidata entry, leaving storageConfigs
	// one longer than chCfg.Disks. The map should still cover the prefix
	// (snapshot's actual disks), not return empty.
	chCfg := &chVMConfig{
		Disks: []chDisk{
			{Path: "/old/base.qcow2"},
			{Path: "/old/data1.raw"},
		},
	}
	storageConfigs := []*types.StorageConfig{
		{Path: "/new/base.qcow2"},
		{Path: "/new/data1.raw"},
		{Path: "/new/cidata.img"}, // appended by ensureCloneCidata
	}
	m := buildStateReplacements(chCfg, storageConfigs)
	if m["/old/base.qcow2"] != "/new/base.qcow2" {
		t.Errorf("base path missing: %v", m)
	}
	if m["/old/data1.raw"] != "/new/data1.raw" {
		t.Errorf("data disk path missing: %v", m)
	}
	if len(m) != 2 {
		t.Errorf("expected 2 entries (chCfg has 2 disks), got %d: %v", len(m), m)
	}
}

func TestRestorePatchStorageConfigs_DropsAppendedCidata(t *testing.T) {
	// Snapshot lacked cidata; ensureCloneCidata appended one. Filter must
	// drop just that cidata, preserving any data disks at the tail-1 spot.
	configs := []*types.StorageConfig{
		{Path: "/o", Role: types.StorageRoleLayer},
		{Path: "/d1", Role: types.StorageRoleData},
		{Path: "/c", Role: types.StorageRoleCidata},
	}
	got := restorePatchStorageConfigs(configs, false, false, false)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	for _, sc := range got {
		if sc.Role == types.StorageRoleCidata {
			t.Errorf("cidata not filtered: %v", got)
		}
	}
}

func TestRestorePatchStorageConfigs_KeepsAllOnDirectBoot(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/l", Role: types.StorageRoleLayer},
		{Path: "/c", Role: types.StorageRoleCOW},
	}
	got := restorePatchStorageConfigs(configs, true, false, false)
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestRestorePatchStorageConfigs_KeepsAllWhenSnapshotHadCidata(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/o", Role: types.StorageRoleCOW},
		{Path: "/c", Role: types.StorageRoleCidata},
	}
	got := restorePatchStorageConfigs(configs, false, false, true)
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestPatchStateJSON_EmptyMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	original := `{"disk_path":"/some/path"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := patchStateJSON(path, map[string]string{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("file changed with empty map: %s", data)
	}
}

// writeCHConfig writes a JSON config to path and returns the path.
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

// readRawJSON reads a JSON file back into a generic map.
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

// baseCHConfig returns a minimal CH config.json with extra fields that CH adds internally.
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

func TestRestoreAndResumeCloneHotplugsCidataByRole(t *testing.T) {
	// Short dir: t.TempDir embeds the test name and busts the unix sockaddr limit.
	sockDir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu    sync.Mutex
		added []string
	)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "vm.add-disk") {
			var d chDisk
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			added = append(added, d.Path)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { _ = srv.Close() })

	// Snapshot lacked cidata, so ensureCloneCidata appended one — and the clone's
	// new --data-disk config landed after it. The hot-plug must pick the Role
	// match, not the last element (which double-attached the data disk and never
	// attached cidata).
	storageConfigs := []*types.StorageConfig{
		{Path: "/store/cow.raw", Role: types.StorageRoleCOW, Serial: "cocoon-cow"},
		{Path: "/run/cidata.img", RO: true, Role: types.StorageRoleCidata},
		{Path: "/run/extra.raw", Role: types.StorageRoleData, Serial: "extra"},
	}
	ch := &CloudHypervisor{}
	if err := ch.restoreAndResumeClone(t.Context(), 0, sock, t.TempDir(), &cloneResumeOpts{
		vmCfg:          &types.VMConfig{Config: types.Config{CPU: 2}},
		storageConfigs: storageConfigs,
		dataDisks:      storageConfigs[2:],
		snapshotCfg:    &chVMConfig{},
	}); err != nil {
		t.Fatalf("restoreAndResumeClone: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"/run/cidata.img", "/run/extra.raw"}
	if !slices.Equal(added, want) {
		t.Fatalf("vm.add-disk paths = %v, want %v", added, want)
	}
}
