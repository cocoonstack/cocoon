package cgroup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestResolveKnobsDefaults(t *testing.T) {
	k := ResolveKnobs(&types.Config{CPU: 2})
	want := Knobs{Weight: 2, QuotaUs: 200000, PeriodUs: 100000, BurstUs: 0}
	if k != want {
		t.Errorf("got %+v, want %+v", k, want)
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
	if err := reconcileFence(parent, "irrelevant", ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != "" {
		t.Errorf("stale fence not cleared: %q err=%v", data, err)
	}
	if err := reconcileFence(parent, "irrelevant", ""); err != nil {
		t.Errorf("steady-state empty reconcile: %v", err)
	}
}
