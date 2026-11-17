package hypervisor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestMigrateBaselineRefusesWithoutReflink(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	if utils.CheckReflink(rec.RunDir) == nil {
		t.Skip("the test filesystem supports reflink")
	}
	calls := &migrateCalls{}
	if _, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{}, migrateStubSpec(calls)); err == nil {
		t.Fatal("capture succeeded without reflink")
	}
	if calls.paused != 0 {
		t.Error("the guest was paused before the refusal")
	}
	if dirs, _ := filepath.Glob(filepath.Join(rec.RunDir, seriesDirPrefix+"*")); len(dirs) != 0 {
		t.Errorf("series dirs left behind: %v", dirs)
	}
}

func TestMigrateMiddleRoundResumesAndHidesTheReference(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	round, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1"}, migrateStubSpec(calls))
	if err != nil {
		skipWithoutFiemap(t, err)
		t.Fatalf("capture: %v", err)
	}
	if round.Round != 1 || round.Paused || calls.resumed != 1 {
		t.Errorf("round %d paused %t resumed %d, want 1 false 1", round.Round, round.Paused, calls.resumed)
	}
	if want := []string{"cow.raw.delta.1", "mem.1", snapshot.SnapshotJSONName}; !slices.Equal(fileNames(round), want) {
		t.Errorf("files = %v, want %v", fileNames(round), want)
	}
	if !utils.FileExists(filepath.Join(dir, "cow.raw.ref")) {
		t.Error("the next round has no reference")
	}
	round, err = b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1"}, migrateStubSpec(calls))
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if round.Round != 2 {
		t.Errorf("round = %d, want 2", round.Round)
	}
}

func TestMigrateFinalRoundLeavesTheGuestPaused(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	round, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1", Final: true}, migrateStubSpec(calls))
	if err != nil {
		skipWithoutFiemap(t, err)
		t.Fatalf("capture: %v", err)
	}
	if !round.Paused || calls.resumed != 0 || calls.ended != 0 {
		t.Errorf("paused %t resumed %d ended %d, want true 0 0", round.Paused, calls.resumed, calls.ended)
	}
	if utils.FileExists(nextRefPath(filepath.Join(dir, types.COWRawFileName))) {
		t.Error("the final round reflinked a reference inside the pause")
	}
	if !slices.Contains(fileNames(round), seriesFinalFile) {
		t.Errorf("files = %v, want the final marker listed for the transfer", fileNames(round))
	}
}

func TestMigrateCaptureRefusesARoundAfterTheFinal(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	if err := os.WriteFile(filepath.Join(dir, seriesFinalFile), nil, 0o600); err != nil {
		t.Fatalf("seed final marker: %v", err)
	}
	for _, req := range []MigrateRequest{{}, {Series: "s1"}, {Series: "s1", Final: true}} {
		calls := &migrateCalls{}
		_, err := b.MigrateCaptureSequence(t.Context(), id, req, migrateStubSpec(calls))
		if err == nil || !strings.Contains(err.Error(), "took its final round") {
			t.Fatalf("capture %+v: %v, want the final-round refusal", req, err)
		}
		if calls.paused != 0 || calls.resumed != 0 || calls.ended != 0 {
			t.Errorf("capture %+v touched the paused guest: %+v", req, calls)
		}
	}
	if !utils.FileExists(dir) {
		t.Error("a refused capture deleted the final series")
	}
}

func TestAFinalSeriesFencesAColdStartAndARestore(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	const id = "vm-handed-off"
	seedFinalSeries(t, seedStoppedVMWithDirs(t, b, id))

	launched := false
	err := b.StartSequence(t.Context(), id, nil, StartSpec{
		Launch: func(context.Context, *VMRecord, string) (int, error) {
			launched = true
			return 4242, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "took its final round") || launched {
		t.Errorf("StartSequence = %v, launched = %t; want the handed-off guest left stopped", err, launched)
	}

	killed := false
	_, err = b.DirectRestoreSequence(t.Context(), id, RestoreSpec{
		VMCfg:     &types.VMConfig{CPU: 1, Memory: 512, Storage: 1024},
		Preflight: func(string, *VMRecord) error { return nil },
		Kill: func(context.Context, string, *VMRecord) error {
			killed = true
			return nil
		},
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "took its final round") || killed {
		t.Errorf("DirectRestoreSequence = %v, killed = %t; want the handed-off guest left alone", err, killed)
	}
}

func TestSweepStaleCaptureDirsKeepsAFinalSeries(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	runDir := t.TempDir()
	final := seedFinalSeries(t, runDir)
	abandoned := filepath.Join(runDir, seriesDirPrefix+"s0")
	if err := os.Mkdir(abandoned, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	past := time.Now().Add(-StaleArtifactGCGrace - time.Hour)
	for _, dir := range []string{final, abandoned} {
		if err := os.Chtimes(dir, past, past); err != nil {
			t.Fatalf("age %s: %v", dir, err)
		}
	}

	for _, err := range b.sweepStaleCaptureDirs(t.Context(), []string{runDir}) {
		t.Fatalf("sweep: %v", err)
	}
	if !utils.FileExists(final) {
		t.Error("gc removed the series that fences a handed-off guest")
	}
	if utils.FileExists(abandoned) {
		t.Error("an abandoned series survived the sweep")
	}
}

func TestRequireFinalSeries(t *testing.T) {
	dir := t.TempDir()
	if err := RequireFinalSeries(dir); err == nil {
		t.Fatal("a series without its final round was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, seriesFinalFile), nil, 0o600); err != nil {
		t.Fatalf("seed final marker: %v", err)
	}
	if err := RequireFinalSeries(dir); err != nil {
		t.Fatalf("RequireFinalSeries: %v", err)
	}
}

func TestMigrateFailedRoundResumesAndDiscardsTheSeries(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	spec := migrateStubSpec(calls)
	spec.Capture = func(*VMRecord, *http.Client, string) error { return errors.New("dump failed") }
	spec.EndSeries = func(_ *VMRecord, hc *http.Client) error {
		calls.ended++
		if calls.resumed == 0 || hc.Timeout != VMMemTransferTimeout {
			t.Error("the series ended on the short timeout before the resume waited out the dump")
		}
		return nil
	}
	if _, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1", Final: true}, spec); err == nil || !strings.Contains(err.Error(), "dump failed") {
		t.Fatalf("capture: %v, want dump failed", err)
	}
	if calls.ended != 1 || calls.resumed != 1 {
		t.Errorf("ended %d resumed %d, want 1 1", calls.ended, calls.resumed)
	}
	if utils.FileExists(dir) {
		t.Error("the failed series survived")
	}
}

func TestMigrateUnknownSeriesKeepsTheGuestRunning(t *testing.T) {
	b, id, _ := newMigrateTestVM(t)
	calls := &migrateCalls{}
	if _, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "gone"}, migrateStubSpec(calls)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture: %v, want not exist", err)
	}
	if calls.paused != 0 || calls.ended != 0 {
		t.Errorf("paused %d ended %d, want 0 0", calls.paused, calls.ended)
	}
}

func TestMigrateAbortRepeatLeavesTheGuestAlone(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	for range 2 {
		if err := b.MigrateAbortSequence(t.Context(), id, "s1", migrateStubSpec(calls)); err != nil {
			t.Fatalf("abort: %v", err)
		}
	}
	if calls.ended != 1 || calls.resumed != 1 {
		t.Errorf("ended %d resumed %d, want 1 1", calls.ended, calls.resumed)
	}
	if utils.FileExists(dir) {
		t.Error("the series survived the abort")
	}
}

func TestMigrateAbortOfAnOldSeriesLeavesTheGuestAlone(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	if err := b.MigrateAbortSequence(t.Context(), id, "old", migrateStubSpec(calls)); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if calls.ended != 0 || calls.resumed != 0 || !utils.FileExists(dir) {
		t.Errorf("ended %d resumed %d kept %t, want 0 0 true", calls.ended, calls.resumed, utils.FileExists(dir))
	}
}

func TestMigrateAbortKeepsTheSeriesUntilTheGuestResumes(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	spec := migrateStubSpec(calls)
	spec.Resume = func(*VMRecord, *http.Client) error { return errors.New("vmm busy") }
	if err := b.MigrateAbortSequence(t.Context(), id, "s1", spec); err == nil {
		t.Fatal("abort hid the failure")
	}
	if !utils.FileExists(dir) {
		t.Fatal("the series went with the guest still paused, so a retry could not resume it")
	}
	if err := b.MigrateAbortSequence(t.Context(), id, "s1", migrateStubSpec(calls)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls.resumed != 1 || utils.FileExists(dir) {
		t.Errorf("resumed %d kept %t, want 1 false", calls.resumed, utils.FileExists(dir))
	}
}

func TestMigrateAbortResumesWhenEndingTheSeriesFails(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	seedSeries(t, rec)
	calls := &migrateCalls{}
	spec := migrateStubSpec(calls)
	spec.EndSeries = func(*VMRecord, *http.Client) error { return errors.New("stop dirty log") }
	if err := b.MigrateAbortSequence(t.Context(), id, "s1", spec); err == nil {
		t.Fatal("abort hid the failure")
	}
	if calls.resumed != 1 {
		t.Errorf("resumed %d, want 1", calls.resumed)
	}
}

func TestMigrateRoundOutOfStepDiscardsTheSeries(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string, spec *MigrateSpec)
	}{
		{
			name: "guest round without a disk round",
			setup: func(_ *testing.T, _ string, spec *MigrateSpec) {
				spec.Rounds = func(string) (int, error) { return 1, nil }
			},
		},
		{
			name: "disk round without a guest round",
			setup: func(t *testing.T, dir string, _ *MigrateSpec) {
				writeSparse(t, filepath.Join(dir, diskDeltaName(types.COWRawFileName, 1)), 8, nil)
			},
		},
		{
			name: "round killed before its reference moved",
			setup: func(t *testing.T, dir string, spec *MigrateSpec) {
				spec.Rounds = func(string) (int, error) { return 1, nil }
				disk := filepath.Join(dir, types.COWRawFileName)
				writeSparse(t, filepath.Join(dir, diskDeltaName(types.COWRawFileName, 1)), 8, nil)
				writeSparse(t, nextRefPath(disk), 8, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, id, rec := newMigrateTestVM(t)
			dir := seedSeries(t, rec)
			calls := &migrateCalls{}
			spec := migrateStubSpec(calls)
			tt.setup(t, dir, &spec)
			_, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1"}, spec)
			if err == nil || !strings.Contains(err.Error(), "start a new series") {
				t.Fatalf("capture: %v, want out of step", err)
			}
			if calls.paused != 0 || calls.ended != 1 || utils.FileExists(dir) {
				t.Errorf("paused %d ended %d kept %t, want 0 1 false", calls.paused, calls.ended, utils.FileExists(dir))
			}
		})
	}
}

func TestMigrateRoundRewritesTheEnvelope(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].NetworkConfigs = []*types.NetworkConfig{{MTU: 1500}, {MTU: 1500}}
		return nil
	}); err != nil {
		t.Fatalf("seed nics: %v", err)
	}
	if _, err := b.MigrateCaptureSequence(t.Context(), id, MigrateRequest{Series: "s1"}, migrateStubSpec(&migrateCalls{})); err != nil {
		skipWithoutFiemap(t, err)
		t.Fatalf("capture: %v", err)
	}
	envelope, err := snapshot.ReadSnapshotEnvelope(dir)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if envelope.Config.NICs != 2 || envelope.Config.ID != "s1" {
		t.Errorf("envelope nics %d id %q, want 2 s1", envelope.Config.NICs, envelope.Config.ID)
	}
}

func TestIsSeriesDirLeavesASnapshotBeingPersisted(t *testing.T) {
	dir := t.TempDir()
	series := filepath.Join(dir, seriesDirPrefix+"s1")
	persisting := filepath.Join(dir, captureDirPrefix+"123")
	for _, d := range []string{series, persisting} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if errs := utils.RemoveMatching(t.Context(), dir, isSeriesDir); len(errs) != 0 {
		t.Fatalf("remove: %v", errs)
	}
	if utils.FileExists(series) || !utils.FileExists(persisting) {
		t.Errorf("series kept %t, persisting snapshot kept %t, want false true", utils.FileExists(series), utils.FileExists(persisting))
	}
}

func TestMigrateAbortOfAStoppedVMDeletesTheSeries(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedVMRecord(t, b, "stopped", 1, 512, 1024, true)
	rec, err := b.LoadRecord(t.Context(), "stopped")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dir := seedSeries(t, &rec)
	calls := &migrateCalls{}
	if err = b.MigrateAbortSequence(t.Context(), "stopped", "s1", migrateStubSpec(calls)); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if calls.resumed != 0 || utils.FileExists(dir) {
		t.Errorf("resumed %d series kept %t, want 0 false", calls.resumed, utils.FileExists(dir))
	}
}

func TestSeriesDirRejectsPathsOutOfTheRunDir(t *testing.T) {
	rec := &VMRecord{RunDir: "/run/vm"}
	for _, series := range []string{"", "..", "../x", "a/b", "/abs"} {
		if _, err := seriesDir(rec, series); err == nil {
			t.Errorf("series %q accepted", series)
		}
	}
	got, err := seriesDir(rec, "123")
	if err != nil || got != "/run/vm/snapshot-migrate-123" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestIsMigrateDeltaFile(t *testing.T) {
	for name, want := range map[string]bool{
		"cow.raw.delta.1":      true,
		"data-x.raw.delta.12":  true,
		"cow.raw.ref":          true,
		"cow.raw.next.ref":     true,
		"series.final":         true,
		"cow.raw":              false,
		"memory-ranges.diff.1": false,
		"state.json":           false,
	} {
		if got := IsMigrateDeltaFile(name); got != want {
			t.Errorf("%s = %t, want %t", name, got, want)
		}
	}
}

type migrateCalls struct {
	paused  int
	resumed int
	ended   int
	rounds  int
}

func migrateStubSpec(calls *migrateCalls) MigrateSpec {
	return MigrateSpec{
		Pause:  func(*VMRecord, *http.Client) error { calls.paused++; return nil },
		Resume: func(*VMRecord, *http.Client) error { calls.resumed++; return nil },
		Capture: func(_ *VMRecord, _ *http.Client, dir string) error {
			calls.rounds++
			return os.WriteFile(filepath.Join(dir, "mem."+strconv.Itoa(calls.rounds)), []byte("x"), 0o600)
		},
		BuildMeta: func(*VMRecord, string) (*snapshot.SnapshotMeta, error) { return &snapshot.SnapshotMeta{}, nil },
		EndSeries: func(*VMRecord, *http.Client) error { calls.ended++; return nil },
		Rounds:    func(string) (int, error) { return calls.rounds, nil },
	}
}

func newMigrateTestVM(t *testing.T) (*Backend, string, *VMRecord) {
	t.Helper()
	b, id := newHibernateTestVM(t)
	rec, err := b.LoadRecord(t.Context(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cow := filepath.Join(rec.RunDir, types.COWRawFileName)
	writeSparse(t, cow, 1<<20, map[int64]string{0: "live"})
	if err := b.dbUpdate(t.Context(), func(idx *VMIndex) error {
		idx.VMs[id].StorageConfigs = []*types.StorageConfig{{Path: cow, Serial: CowSerial, Role: types.StorageRoleCOW}}
		return nil
	}); err != nil {
		t.Fatalf("seed disks: %v", err)
	}
	return b, id, &rec
}

func seedSeries(t *testing.T, rec *VMRecord) string {
	t.Helper()
	dir := filepath.Join(rec.RunDir, seriesDirPrefix+"s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSparse(t, filepath.Join(dir, types.COWRawFileName), 1<<20, map[int64]string{0: "base"})
	return dir
}

func seedFinalSeries(t *testing.T, runDir string) string {
	t.Helper()
	dir := filepath.Join(runDir, seriesDirPrefix+"s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, seriesFinalFile), nil, 0o600); err != nil {
		t.Fatalf("seed final marker: %v", err)
	}
	return dir
}

func fileNames(round *MigrateRound) []string {
	names := make([]string, len(round.Files))
	for i, f := range round.Files {
		names[i] = f.Name
	}
	return names
}

func skipWithoutFiemap(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("needs FIEMAP")
	}
}
