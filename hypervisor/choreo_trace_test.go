package hypervisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/gc"
	meteringcapture "github.com/cocoonstack/cocoon/metering/capture"
	"github.com/cocoonstack/cocoon/types"
)

// TestLegacyChoreographyTrace is the P0 differential gate (design §10): the migrated boundary must reproduce the legacy run's returned values, error identities and final bytes step for step.
func TestLegacyChoreographyTrace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "legacy-choreo-trace.json"))
	if err != nil {
		t.Fatalf("golden: %v", err)
	}
	var golden choreoTrace
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("golden decode: %v", err)
	}

	const typ = "test-hv"
	dir := t.TempDir()
	rec := meteringcapture.New()
	b := &Backend{
		Typ:      typ,
		NS:       VMNamespaceName(typ),
		Conf:     meteringStubConfig{stubBackendConfig: stubBackendConfig{rootDir: dir}, vmRunRoot: dir},
		Meta:     testNamespace(t, typ, dir),
		Metering: rec,
	}
	ctx := t.Context()

	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	origNow := timeNow
	timeNow = func() time.Time { return clock }
	t.Cleanup(func() { timeNow = origNow })

	classify := func(err error) (string, string) {
		switch {
		case err == nil:
			return "nil", ""
		case errors.Is(err, ErrNotFound):
			return "notfound", err.Error()
		default:
			return "other", err.Error()
		}
	}
	var steps []choreoStep
	record := func(op, result string, err error) {
		cls, msg := classify(err)
		var kinds []string
		for _, e := range rec.Entries() {
			kinds = append(kinds, string(e.Kind)+"/"+string(e.Reason))
		}
		rec.Reset()
		steps = append(steps, choreoStep{Op: op, ErrClass: cls, ErrMsg: msg, Result: result, Metering: kinds})
	}
	loadJSON := func(id string) string {
		r, err := b.LoadRecord(ctx, id)
		if err != nil {
			return "load-err:" + err.Error()
		}
		buf, _ := json.Marshal(r)
		return string(buf)
	}
	recordLoad := func(op, id string, err error) {
		if err != nil {
			record(op, "", err)
			return
		}
		record(op, loadJSON(id), nil)
	}

	cfgAlpha := &types.VMConfig{Name: "alpha", Config: types.Config{Image: "img:1", CPU: 2, Memory: 1 << 30, Storage: 10 << 30}}
	record("reserve-vm1", "", b.ReserveVM(ctx, "VM1", cfgAlpha, map[string]struct{}{"blobA": {}}, "/r/VM1", "/l/VM1"))
	record("reserve-name-collision", "", b.ReserveVM(ctx, "VM2", cfgAlpha, nil, "/r/VM2", "/l/VM2"))
	recordLoad("reserve-adopt", "VM1", b.ReserveVM(ctx, "VM1", cfgAlpha, map[string]struct{}{"blobA": {}, "blobB": {}}, "/r2/VM1", "/l2/VM1"))
	cfgGamma := &types.VMConfig{Name: "gamma", Config: types.Config{CPU: 1}}
	record("reserve-id-collision", "", b.ReserveVM(ctx, "VM1", cfgGamma, nil, "/r/VM1", "/l/VM1"))
	info := &types.VM{ID: "VM1", Hypervisor: typ, State: types.VMStateStopped, Config: *cfgAlpha}
	recordLoad("finalize", "VM1", b.FinalizeCreate(ctx, "VM1", info, nil, map[string]struct{}{"blobA": {}, "blobB": {}}))
	id, err := b.ResolveRef(ctx, "alpha")
	record("resolve-name", id, err)
	_, err = b.ResolveRef(ctx, "ghost")
	record("resolve-missing", "", err)
	recordLoad("resize", "VM1", b.UpdateRecord(ctx, "VM1", func(r *VMRecord) error {
		r.Config.CPU = 4
		r.Config.Storage = 20 << 30
		return nil
	}))
	recordLoad("mark-started", "VM1", b.BatchMarkStarted(ctx, []string{"VM1"}))
	recordLoad("update-stopped", "VM1", b.UpdateStates(ctx, []string{"VM1"}, types.VMStateStopped))
	cfgBeta := &types.VMConfig{Name: "beta", Config: types.Config{CPU: 1}}
	record("reserve-vm3", "", b.ReserveVM(ctx, "VM3", cfgBeta, nil, "/r/VM3", "/l/VM3"))
	b.RollbackCreate(ctx, "VM3", "beta")
	_, err = b.ResolveRef(ctx, "beta")
	record("rollback-then-resolve", "", err)

	vm1, err := b.LoadRecord(ctx, "VM1")
	if err != nil {
		t.Fatalf("load VM1 pre-delete: %v", err)
	}
	record("delete", "", b.deleteVMProtocol(ctx, "VM1", &vm1))
	_, err = b.ResolveRef(ctx, "alpha")
	record("delete-then-resolve", "", err)

	cfgDelta := &types.VMConfig{Name: "delta", Config: types.Config{CPU: 1}}
	record("reserve-vm4", "", b.ReserveVM(ctx, "VM4", cfgDelta, nil, t.TempDir(), t.TempDir()))
	clock = clock.Add(25 * time.Hour)
	orch := gc.New()
	b.RegisterGC(orch)
	record("gc-pass", "", orch.Run(ctx))
	_, err = b.ResolveRef(ctx, "delta")
	record("gc-pass-then-resolve", "", err)

	cfgEps := &types.VMConfig{Name: "epsilon", Config: types.Config{Image: "img:2", CPU: 1, Memory: 1 << 29, Storage: 5 << 30}}
	recordLoad("reserve-vm5", "VM5", b.ReserveVM(ctx, "VM5", cfgEps, map[string]struct{}{"blobC": {}}, "/r/VM5", "/l/VM5"))

	if len(steps) != len(golden.Steps) {
		t.Fatalf("step count drift: got %d, legacy %d", len(steps), len(golden.Steps))
	}
	for i, g := range golden.Steps {
		got := steps[i]
		if got.Op != g.Op || got.ErrClass != g.ErrClass || got.ErrMsg != g.ErrMsg || got.Result != g.Result {
			t.Errorf("step %s drift:\n got: %+v\nwant: %+v", g.Op, got, g)
		}
		if len(got.Metering) != len(g.Metering) {
			t.Errorf("step %s metering drift: got %v, legacy %v", g.Op, got.Metering, g.Metering)
			continue
		}
		for j := range g.Metering {
			if got.Metering[j] != g.Metering[j] {
				t.Errorf("step %s metering[%d]: got %s, legacy %s", g.Op, j, got.Metering[j], g.Metering[j])
			}
		}
	}

	final, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("final bytes: %v", err)
	}
	if got := string(final); got != golden.FinalBytes {
		t.Errorf("final namespace bytes drift:\n got: %s\nwant: %s", got, golden.FinalBytes)
	}
}

type choreoStep struct {
	Op       string   `json:"op"`
	ErrClass string   `json:"err_class"`
	ErrMsg   string   `json:"err_msg,omitempty"`
	Result   string   `json:"result,omitempty"`
	Metering []string `json:"metering,omitempty"`
}

type choreoTrace struct {
	Steps      []choreoStep `json:"steps"`
	FinalBytes string       `json:"final_bytes"`
}
