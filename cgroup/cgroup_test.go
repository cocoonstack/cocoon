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

func TestResolveKnobsDefaults(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 2})
	want := Knobs{Weight: 2, QuotaUs: 200000, PeriodUs: 100000, BurstUs: 200000, BurstDefaulted: true}
	if k != want {
		t.Errorf("got %+v, want %+v", k, want)
	}
}

func TestResolveKnobsDefaultBurstFollowsExplicitQuota(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 4, CPUQuotaUs: 50000})
	if k.BurstUs != 50000 || !k.BurstDefaulted {
		t.Errorf("got burst %d defaulted %v, want 50000 true", k.BurstUs, k.BurstDefaulted)
	}
}

func TestResolveKnobsBurstNone(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 2, CPUBurstUs: -1})
	if k.BurstUs != 0 || k.BurstDefaulted {
		t.Errorf("got burst %d defaulted %v, want 0 false", k.BurstUs, k.BurstDefaulted)
	}
}

func TestResolveKnobsOverrides(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 4, CPUWeight: 100, CPUQuotaUs: 50000, CPUPeriodUs: 20000, CPUBurstUs: 10000})
	want := Knobs{Weight: 100, QuotaUs: 50000, PeriodUs: 20000, BurstUs: 10000}
	if k != want {
		t.Errorf("got %+v, want %+v", k, want)
	}
}

func TestResolveKnobsDefaultQuotaUsesExplicitPeriod(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 2, CPUPeriodUs: 50000})
	if k.QuotaUs != 100000 {
		t.Errorf("quota %d, want CPU x period = 100000", k.QuotaUs)
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
		if err := os.Mkdir(filepath.Join(parent, dir), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "vm-C.scope"), nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

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
	if err := os.Mkdir(ScopeDir(parent, "X"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := RemoveEmpty(parent, "X"); err != nil {
		t.Errorf("empty scope: %v", err)
	}
	if err := RemoveEmpty(parent, "X"); err != nil {
		t.Errorf("missing scope: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(ScopeDir(parent, "Y"), "child"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
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

func TestKnobsValidateRejectsBadCPUSet(t *testing.T) {
	k := Knobs{Weight: 1, QuotaUs: 100000, PeriodUs: 100000, CPUSet: "9-1"}
	if err := k.Validate(); err == nil {
		t.Error("want error for invalid cpu list")
	}
}

func TestCheckSubset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuset.cpus.effective"), []byte("0-14\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
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
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte(placement+"\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
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
	if err := os.WriteFile(path, []byte("0-14\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
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
	if err := os.WriteFile(filepath.Join(parent, "cpuset.cpus"), []byte("0-7\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// No cpuset.cpus.effective fixture exists: reaching checkSubset would fail, so success proves the parsed-set gate short-circuited.
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

	if err := os.Mkdir(ScopeDir(parent, "X"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := explainFenceWrite(parent, "0-14", "", enospc)
	if !errors.Is(got, enospc) {
		t.Fatalf("wrapped error dropped the cause: %v", got)
	}
	if !strings.Contains(got.Error(), "1 VM(s) running") {
		t.Errorf("message does not name the live VMs: %v", got)
	}
}

func TestPlaceScopeReadGate(t *testing.T) {
	parent := t.TempDir()
	dir := ScopeDir(parent, "X")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte("2-3\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := placeScope(parent, dir, "2,3"); err != nil {
		t.Errorf("equal placement rewrote: %v", err)
	}
}

func TestEffectiveCPUs(t *testing.T) {
	if got := EffectiveCPUs("2-3", "0-14"); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("placement wins: got %v", got)
	}
	if got := EffectiveCPUs("", "0-1"); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("fence fallback: got %v", got)
	}
	if EffectiveCPUs("", "") != nil {
		t.Error("no constraint: want nil")
	}
}

func TestArmSetsQuotaThenBurst(t *testing.T) {
	parent := t.TempDir()
	dir := ScopeDir(parent, "A")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, f := range []string{"cpu.max", "cpu.max.burst"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if err := Arm(parent, "A", Knobs{QuotaUs: 150000, PeriodUs: 100000, BurstUs: 50000}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	max, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	burst, _ := os.ReadFile(filepath.Join(dir, "cpu.max.burst"))
	if string(max) != "150000 100000" || string(burst) != "50000" {
		t.Errorf("max=%q burst=%q", max, burst)
	}
}

func TestArmDefaultedBurstDegradesWhenKernelLacksBurst(t *testing.T) {
	parent := t.TempDir()
	dir := ScopeDir(parent, "A")
	danglingBurstFile(t, dir)
	if err := Arm(parent, "A", Knobs{QuotaUs: 200000, PeriodUs: 100000, BurstUs: 200000, BurstDefaulted: true}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	max, _ := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if string(max) != "200000 100000" {
		t.Errorf("max=%q, want quota still armed", max)
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
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone", "cpu.max.burst"), filepath.Join(dir, "cpu.max.burst")); err != nil {
		t.Fatalf("setup: %v", err)
	}
}
