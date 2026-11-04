package hypervisor

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if dirs, _ := filepath.Glob(filepath.Join(rec.RunDir, captureDirPrefix+"*")); len(dirs) != 0 {
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
	if want := []string{"cow.raw.delta.1", "mem.1"}; !slices.Equal(fileNames(round), want) {
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
	if utils.FileExists(filepath.Join(dir, "cow.raw.ref")) {
		t.Error("the final round reflinked a reference inside the pause")
	}
}

func TestMigrateFailedRoundResumesAndDiscardsTheSeries(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	spec := migrateStubSpec(calls)
	spec.Capture = func(*VMRecord, *http.Client, string) error { return errors.New("dump failed") }
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

func TestMigrateAbortIsIdempotent(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	for range 2 {
		if err := b.MigrateAbortSequence(t.Context(), id, "s1", migrateStubSpec(calls)); err != nil {
			t.Fatalf("abort: %v", err)
		}
	}
	if calls.ended != 2 || calls.resumed != 2 {
		t.Errorf("ended %d resumed %d, want 2 2", calls.ended, calls.resumed)
	}
	if utils.FileExists(dir) {
		t.Error("the series survived the abort")
	}
}

func TestMigrateAbortResumesWhenEndingTheSeriesFails(t *testing.T) {
	b, id, rec := newMigrateTestVM(t)
	dir := seedSeries(t, rec)
	calls := &migrateCalls{}
	spec := migrateStubSpec(calls)
	spec.EndSeries = func(*VMRecord, *http.Client) error { return errors.New("stop dirty log") }
	if err := b.MigrateAbortSequence(t.Context(), id, "s1", spec); err == nil {
		t.Fatal("abort hid the failure")
	}
	if calls.resumed != 1 || utils.FileExists(dir) {
		t.Errorf("resumed %d series kept %t, want 1 false", calls.resumed, utils.FileExists(dir))
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
	if err != nil || got != "/run/vm/snapshot-123" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestIsMigrateDeltaFile(t *testing.T) {
	for name, want := range map[string]bool{
		"cow.raw.delta.1":      true,
		"data-x.raw.delta.12":  true,
		"cow.raw.ref":          true,
		"cow.raw.next.ref":     true,
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
			return os.WriteFile(filepath.Join(dir, "mem."+string(rune('0'+calls.rounds))), []byte("x"), 0o600)
		},
		BuildMeta: func(*VMRecord, string) (*snapshot.SnapshotMeta, error) { return &snapshot.SnapshotMeta{}, nil },
		EndSeries: func(*VMRecord, *http.Client) error { calls.ended++; return nil },
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
	dir := filepath.Join(rec.RunDir, captureDirPrefix+"s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSparse(t, filepath.Join(dir, types.COWRawFileName), 1<<20, map[int64]string{0: "base"})
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
