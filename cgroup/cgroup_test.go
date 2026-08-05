package cgroup

import (
	"os"
	"path/filepath"
	"slices"
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
