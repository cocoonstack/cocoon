package cgroup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestResolveKnobs(t *testing.T) {
	tests := []struct {
		name string
		cfg  types.Config
		want Knobs
	}{
		{"defaults", types.Config{CPU: 2}, Knobs{Weight: 2, QuotaUs: 200000, PeriodUs: 100000, BurstUs: 200000, BurstDefaulted: true}},
		{"default burst follows explicit quota", types.Config{CPU: 4, CPUQuotaUs: 50000}, Knobs{Weight: 4, QuotaUs: 50000, PeriodUs: 100000, BurstUs: 50000, BurstDefaulted: true}},
		{"burst none", types.Config{CPU: 2, CPUBurstUs: -1}, Knobs{Weight: 2, QuotaUs: 200000, PeriodUs: 100000}},
		{"overrides", types.Config{CPU: 4, CPUWeight: 100, CPUQuotaUs: 50000, CPUPeriodUs: 20000, CPUBurstUs: 10000}, Knobs{Weight: 100, QuotaUs: 50000, PeriodUs: 20000, BurstUs: 10000}},
		{"default quota uses explicit period", types.Config{CPU: 2, CPUPeriodUs: 50000}, Knobs{Weight: 2, QuotaUs: 100000, PeriodUs: 50000, BurstUs: 100000, BurstDefaulted: true}},
		{"auto cpuset passes through", types.Config{CPU: 2, CPUSetCPUs: AutoCPUSet}, Knobs{Weight: 2, QuotaUs: 200000, PeriodUs: 100000, BurstUs: 200000, BurstDefaulted: true, CPUSet: AutoCPUSet}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveKnobs(&tt.cfg); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKnobsValidate(t *testing.T) {
	tests := []struct {
		name    string
		k       Knobs
		wantErr bool
	}{
		{"defaults for 1 cpu", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000}, false},
		{"burst at quota", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, BurstUs: 100000}, false},
		{"weight too high", Knobs{Weight: 10001, QuotaUs: 100000, PeriodUs: 100000}, true},
		{"weight zero", Knobs{Weight: 0, QuotaUs: 100000, PeriodUs: 100000}, true},
		{"period below kernel min", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 999}, true},
		{"period above kernel max", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 1000001}, true},
		{"quota below kernel min", Knobs{Weight: 1, QuotaUs: 999, PeriodUs: 100000}, true},
		{"burst above quota", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, BurstUs: 100001}, true},
		{"negative burst", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, BurstUs: -1}, true},
		{"auto cpuset", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, CPUSet: AutoCPUSet}, false},
		{"bad cpuset", Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, CPUSet: "9-1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.k.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestListScopeVMIDs(t *testing.T) {
	parent := t.TempDir()
	for _, dir := range []string{"vm-A.scope", "vm-B.scope", "other-dir"} {
		mkdir(t, filepath.Join(parent, dir))
	}
	writeFile(t, filepath.Join(parent, "vm-C.scope"), "")

	ids, err := ListScopeVMIDs(parent)
	if err != nil {
		t.Fatalf("ListScopeVMIDs: %v", err)
	}
	slices.Sort(ids)
	if want := []string{"A", "B"}; !slices.Equal(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}

	ids, err = ListScopeVMIDs(filepath.Join(parent, "missing"))
	if err != nil || ids != nil {
		t.Errorf("missing parent: got %v, %v; want nil, nil", ids, err)
	}
}

func TestRemoveEmpty(t *testing.T) {
	parent := t.TempDir()
	mkdir(t, ScopeDir(parent, "X"))
	if err := RemoveEmpty(parent, "X"); err != nil {
		t.Errorf("empty scope: %v", err)
	}
	if err := RemoveEmpty(parent, "X"); err != nil {
		t.Errorf("missing scope: %v", err)
	}

	mkdir(t, filepath.Join(ScopeDir(parent, "Y"), "child"))
	if err := RemoveEmpty(parent, "Y"); err == nil {
		t.Error("populated scope: want error, got nil")
	}
}

func TestParseStat(t *testing.T) {
	stat := parseStat("usage_usec 1000\nnr_throttled 3\nthrottled_usec 250\nbad line here\n")
	if stat["nr_throttled"] != 3 || stat["throttled_usec"] != 250 {
		t.Errorf("got %v", stat)
	}
}

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "3", want: []int{3}},
		{in: "0-3", want: []int{0, 1, 2, 3}},
		{in: "0,2-4,7", want: []int{0, 2, 3, 4, 7}},
		{in: "4-2", wantErr: true},
		{in: "a-b", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "1,,2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCPUList(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatCPUList(t *testing.T) {
	tests := []struct {
		cpus []int
		want string
	}{
		{cpus: nil, want: ""},
		{cpus: []int{3}, want: "3"},
		{cpus: []int{0, 1, 2, 3, 192, 193, 194, 195}, want: "0-3,192-195"},
		{cpus: []int{0, 2, 3, 7}, want: "0,2-3,7"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := FormatCPUList(tt.cpus); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			back, err := ParseCPUList(tt.want)
			if err != nil || !slices.Equal(back, tt.cpus) {
				t.Errorf("round trip = %v, %v", back, err)
			}
		})
	}
}

func TestCheckSubset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cpuset.cpus.effective"), "0-14\n")
	if err := checkSubset("2-4", dir, "fence"); err != nil {
		t.Errorf("subset rejected: %v", err)
	}
	if err := checkSubset("14-15", dir, "fence"); err == nil {
		t.Error("want error: cpu 15 outside effective 0-14")
	}
}

func TestCheckScopePlacements(t *testing.T) {
	parent := t.TempDir()
	mk := func(id, placement string) {
		dir := ScopeDir(parent, id)
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, "cpuset.cpus"), placement+"\n")
	}
	mk("A", "")
	mk("B", "2-3")

	if err := checkScopePlacements(parent, "0-7"); err != nil {
		t.Errorf("fence covering placements rejected: %v", err)
	}
	if err := checkScopePlacements(parent, "0-2"); err == nil {
		t.Error("want error: fence 0-2 excludes cpu 3 used by B")
	}
}

func TestReconcileFenceClearsStaleValue(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "cpuset.cpus")
	writeFile(t, path, "0-14\n")
	if err := reconcileFence(parent, ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "" {
		t.Errorf("stale fence not cleared: %q err=%v", data, err)
	}
	if err := reconcileFence(parent, ""); err != nil {
		t.Errorf("steady-state empty reconcile: %v", err)
	}
}

func TestReconcileFenceCanonicalEquality(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "cpuset.cpus"), "0-7\n")

	if err := reconcileFence(parent, "0-3,4-7"); err != nil {
		t.Errorf("canonically-equal fence rewrote: %v", err)
	}
}

func TestExplainFenceWriteNamesLiveVMs(t *testing.T) {
	parent := t.TempDir()
	enospc := errors.New("no space left on device")

	if got := explainFenceWrite(parent, "0-14", "", enospc); got != enospc {
		t.Errorf("no live scope: got %v, want the error unchanged", got)
	}
	if got := explainFenceWrite(parent, "0-14", "", nil); got != nil {
		t.Errorf("successful write: got %v, want nil", got)
	}

	mkdir(t, ScopeDir(parent, "X"))
	got := explainFenceWrite(parent, "0-14", "", enospc)
	if !errors.Is(got, enospc) {
		t.Fatalf("wrapped error dropped the cause: %v", got)
	}
	if !strings.Contains(got.Error(), "1 VM(s) running") {
		t.Errorf("message does not name the live VMs: %v", got)
	}
}

func TestArmWritesQuotaAndBurst(t *testing.T) {
	parent := t.TempDir()
	dir := ScopeDir(parent, "A")
	mkdir(t, dir)
	for _, f := range []string{"cpu.max", "cpu.max.burst"} {
		writeFile(t, filepath.Join(dir, f), "")
	}
	if err := Arm(parent, "A", Knobs{QuotaUs: 150000, PeriodUs: 100000, BurstUs: 50000}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	cpuMax, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	burst, _ := os.ReadFile(filepath.Join(dir, "cpu.max.burst"))
	if string(cpuMax) != "150000 100000" || string(burst) != "50000" {
		t.Errorf("max=%q burst=%q", cpuMax, burst)
	}
}

func TestArmDefaultedBurstDegradesWhenKernelLacksBurst(t *testing.T) {
	parent := t.TempDir()
	dir := ScopeDir(parent, "A")
	danglingBurstFile(t, dir)
	if err := Arm(parent, "A", Knobs{QuotaUs: 200000, PeriodUs: 100000, BurstUs: 200000, BurstDefaulted: true}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	cpuMax, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if string(cpuMax) != "200000 100000" {
		t.Errorf("max=%q, want quota still armed", cpuMax)
	}
}

func TestArmExplicitBurstFailsWhenKernelLacksBurst(t *testing.T) {
	parent := t.TempDir()
	danglingBurstFile(t, ScopeDir(parent, "A"))
	if err := Arm(parent, "A", Knobs{QuotaUs: 200000, PeriodUs: 100000, BurstUs: 100000}); err == nil {
		t.Error("want error for explicit burst without kernel support")
	}
}

func danglingBurstFile(t *testing.T, dir string) {
	t.Helper()
	mkdir(t, dir)
	writeFile(t, filepath.Join(dir, "cpu.max"), "")
	if err := os.Symlink(filepath.Join(dir, "gone", "cpu.max.burst"), filepath.Join(dir, "cpu.max.burst")); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
}
