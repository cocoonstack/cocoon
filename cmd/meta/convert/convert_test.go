package convert

import (
	"bufio"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/utils"
)

var testTables = []string{"records", "names", "tombstones"}

func TestConvertRefusesARunningDaemon(t *testing.T) {
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	if err := os.MkdirAll(filepath.Dir(spec.DaemonLock), 0o750); err != nil {
		t.Fatal(err)
	}
	daemon := flock.New(spec.DaemonLock)
	if ok, err := daemon.TryLock(t.Context()); err != nil || !ok {
		t.Fatalf("hold daemon lock: ok=%v err=%v", ok, err)
	}
	err := Run(t.Context(), spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "hold the meta store") {
		t.Fatalf("want refusal while a daemon holds only its instance lock, got %v", err)
	}
	if !utils.FileExists(spec.JSON[0].FilePath) || utils.FileExists(spec.DBPath) {
		t.Fatal("the refused convert touched the stores")
	}
	if err := daemon.Unlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), spec, "sqlite"); err != nil {
		t.Fatalf("convert after the daemon stopped: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms", "images")
	seedJSON(t, spec, "vms")
	seedJSON(t, spec, "images")

	if err := Run(ctx, spec, "sqlite"); err != nil {
		t.Fatalf("convert to sqlite: %v", err)
	}
	if utils.FileExists(spec.JSON[0].FilePath) {
		t.Fatal("json source not retired aside")
	}
	sq := openSQLite(t, spec)
	if got := scanAll(t, sq, "vms"); len(got) != 4 || got["records/id1"] != `{"v":1}` || got["names/alpha"] != `"id1"` {
		t.Fatalf("sqlite content: %v", got)
	}
	putRaw(t, sq, "vms", "records", "id3", `{"v":3}`)
	if err := sq.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	if err := Run(ctx, spec, "json"); err != nil {
		t.Fatalf("convert back to json: %v", err)
	}
	if utils.FileExists(spec.DBPath) {
		t.Fatal("sqlite source not retired aside")
	}
	js := openJSON(t, spec)
	got := scanAll(t, js, "vms")
	if len(got) != 5 || got["records/id3"] != `{"v":3}` || got["tombstones/id9"] != `{"dead":true}` {
		t.Fatalf("json content after round trip: %v", got)
	}
}

func TestDanglingNameRefused(t *testing.T) {
	spec := testSpec(t, "vms")
	s := openJSON(t, spec)
	putRaw(t, s, "vms", "names", "ghost", `"missing"`)
	_ = s.Close()
	err := Run(t.Context(), spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "missing record") {
		t.Fatalf("want dangling-name refusal, got %v", err)
	}
}

func TestResumeClaimsCommittedTarget(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	src := beginConversion(t, spec)
	dst, err := openTarget(ctx, spec, "sqlite")
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if err := copyNamespace(ctx, src, dst, spec.Decls[0]); err != nil {
		t.Fatalf("copy: %v", err)
	}
	putRaw(t, src, "vms", "records", "late", `{}`)
	_ = src.Close()
	_ = dst.Close()

	if err := Run(ctx, spec, "sqlite"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	sq := openSQLite(t, spec)
	if got := scanAll(t, sq, "vms"); len(got) != 4 {
		t.Fatalf("content after resume: %v", got)
	}
}

func TestSourceChangedRefused(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	src := beginConversion(t, spec)
	putRaw(t, src, "vms", "records", "late", `{}`)
	_ = src.Close()

	err := Run(ctx, spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "source changed") || !strings.Contains(err.Error(), spec.DBPath) {
		t.Fatalf("want source-changed refusal naming the partial target, got %v", err)
	}
}

func TestMissingJSONSourceRefused(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")

	err := Run(ctx, spec, "sqlite")

	if err == nil || !strings.Contains(err.Error(), "to convert from") {
		t.Fatalf("want missing-source refusal, got %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(spec.MetaRoot, "*manifest*")); len(left) != 0 {
		t.Fatalf("manifest written before the source was checked: %v", left)
	}
}

func TestTargetNotFreshRefused(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	if err := metasqlite.Init(ctx, spec.DBPath, spec.Decls...); err != nil {
		t.Fatalf("init: %v", err)
	}
	sq := openSQLite(t, spec)
	putRaw(t, sq, "vms", "records", "foreign", `{}`)
	_ = sq.Close()

	err := Run(ctx, spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want existing-target refusal, got %v", err)
	}
	if utils.FileExists(filepath.Join(spec.MetaRoot, metasqlite.ManifestName)) {
		t.Fatal("manifest written for a refused target")
	}
	sq = openSQLite(t, spec)
	if got := scanAll(t, sq, "vms"); len(got) != 1 || got["records/foreign"] != `{}` {
		t.Fatalf("refused convert wrote into the target: %v", got)
	}
}

func TestJSONTargetNotFreshRefused(t *testing.T) {
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	if err := Run(t.Context(), spec, "sqlite"); err != nil {
		t.Fatalf("forward convert: %v", err)
	}
	seedJSON(t, spec, "vms")

	err := Run(t.Context(), spec, "json")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want existing-target refusal, got %v", err)
	}
	if utils.FileExists(filepath.Join(spec.MetaRoot, metasqlite.ManifestName)) {
		t.Fatal("manifest written for a refused target")
	}
}

func TestOrdinaryOpenRefusedDuringConversion(t *testing.T) {
	spec := testSpec(t, "vms")
	if err := os.MkdirAll(spec.MetaRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.MetaRoot, metasqlite.ManifestName), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := metasqlite.Open(spec.DBPath, spec.Decls...); err == nil || !strings.Contains(err.Error(), "conversion is in flight") {
		t.Fatalf("want in-flight refusal, got %v", err)
	}
	if err := metasqlite.Init(t.Context(), spec.DBPath, spec.Decls...); err == nil || !strings.Contains(err.Error(), "conversion is in flight") {
		t.Fatalf("want init refusal, got %v", err)
	}
}

func TestConvertRefusesUnsupportedFS(t *testing.T) {
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	t.Setenv("COCOON_TEST_UNSUPPORTED_FS", "nfs")
	err := Run(t.Context(), spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "unsupported filesystem") {
		t.Fatalf("want fs refusal, got %v", err)
	}
	if utils.FileExists(spec.DBPath) {
		t.Fatal("convert created target WAL state on refused filesystem")
	}
	if err := metasqlite.RefuseManifest(spec.DBPath); err != nil {
		t.Fatalf("the refused convert locked the root: %v", err)
	}
}

func TestCrashRerunMatrix(t *testing.T) {
	forward := []string{"manifest-saved", "target-opened", "ns-copied", "ns-marked", "ns-done", "source-renamed"}
	reverse := []string{"manifest-saved", "target-opened", "ns-copied", "ns-done", "checkpointed", "source-renamed"}

	run := func(t *testing.T, target string, steps []string) {
		for _, step := range steps {
			t.Run(target+"/"+step, func(t *testing.T) {
				spec := testSpec(t, "vms")
				seedJSON(t, spec, "vms")
				if target == "json" {
					if err := Run(t.Context(), spec, "sqlite"); err != nil {
						t.Fatalf("forward convert: %v", err)
					}
				}
				runCrashingAt(t, spec, target, step)
				if rerr := Run(t.Context(), spec, target); rerr != nil {
					t.Fatalf("rerun after crash at %s: %v", step, rerr)
				}
				open := openJSON
				if target == "sqlite" {
					open = openSQLite
				}
				store := open(t, spec)
				if got := scanAll(t, store, "vms"); len(got) != 4 || got["records/id1"] != `{"v":1}` {
					t.Fatalf("content after rerun at %s: %v", step, got)
				}
			})
		}
	}
	run(t, "sqlite", forward)
	run(t, "json", reverse)
}

func TestDistinctGenerationsRoundTrip(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")

	js := openJSON(t, spec)
	for gen := 1; gen <= 2; gen++ {
		putRaw(t, js, "vms", "records", "id1", `{"gen":`+strconv.Itoa(gen)+`}`)
	}
	_ = js.Close()
	if !utils.FileExists(spec.JSON[0].FilePath + ".prev") {
		t.Fatal("precondition: .prev generation missing")
	}

	if err := Run(ctx, spec, "sqlite"); err != nil {
		t.Fatalf("convert to sqlite: %v", err)
	}
	for _, f := range []string{spec.JSON[0].FilePath, spec.JSON[0].FilePath + ".prev"} {
		if utils.FileExists(f) {
			t.Fatalf("json generation %s not retired aside", f)
		}
	}
	if err := Run(ctx, spec, "json"); err != nil {
		t.Fatalf("convert back to json: %v", err)
	}

	main, err := os.ReadFile(spec.JSON[0].FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.JSON[0].FilePath, append(main, []byte("garbage")...), 0o644); err != nil {
		t.Fatal(err)
	}
	js2 := openJSON(t, spec)
	if got := scanAll(t, js2, "vms"); got["records/id1"] != `{"gen":2}` {
		t.Fatalf("served generation after torn main: %v", got)
	}
}

func TestUncheckpointedWALReverseConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process gate skipped in -short")
	}
	if os.Getenv("META_MP_DIR") != "" {
		t.Skip("helper process only")
	}
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	if err := Run(t.Context(), spec, "sqlite"); err != nil {
		t.Fatalf("forward convert: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestWALWriterWorker$", "-test.count=1")
	cmd.Env = append(os.Environ(), "META_MP_DIR="+spec.MetaRoot, "META_MP_DB="+spec.DBPath)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "ACK") {
			break
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if st, werr := os.Stat(spec.DBPath + "-wal"); werr != nil || st.Size() == 0 {
		t.Fatalf("precondition: WAL empty (err=%v), the gate would be vacuous", werr)
	}

	if err := Run(t.Context(), spec, "json"); err != nil {
		t.Fatalf("reverse convert: %v", err)
	}
	js := openJSON(t, spec)
	if got := scanAll(t, js, "vms"); got["records/walrec"] != `{"wal":1}` {
		t.Fatalf("WAL-stranded commit lost across reverse conversion: %v", got)
	}
}

func TestWALWriterWorker(t *testing.T) {
	db := os.Getenv("META_MP_DB")
	if db == "" {
		t.Skip("helper process only")
	}
	decls := []metasqlite.Namespace{{Name: "vms", Tables: testTables}}
	s, err := metasqlite.Open(db, decls...)
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, s, "vms", "records", "walrec", `{"wal":1}`)
	fmt.Println("ACK")
	select {}
}

func TestConvertSkipsNeverWrittenNamespace(t *testing.T) {
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	late := metajson.Namespace{
		Name:     "latecomer",
		FilePath: filepath.Join(spec.MetaRoot, "..", "notready", "late.json"),
		LockPath: filepath.Join(spec.MetaRoot, "..", "notready", "late.lock"),
		Codec:    metajson.GenericCodec{},
	}
	spec.JSON = append(spec.JSON, late)
	spec.Decls = append(spec.Decls, metasqlite.Namespace{Name: "latecomer", Tables: testTables})
	if err := Run(t.Context(), spec, "sqlite"); err != nil {
		t.Fatalf("convert with never-written namespace: %v", err)
	}
	sq := openSQLite(t, spec)
	if got := scanAll(t, sq, "vms"); len(got) != 4 {
		t.Fatalf("content: %v", got)
	}
	_ = sq.Close()
	if err := Run(t.Context(), spec, "json"); err != nil {
		t.Fatalf("reverse convert with never-written namespace: %v", err)
	}
}

func TestConvertRerunsAfterCrashedTargetInit(t *testing.T) {
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")
	runCrashingAt(t, spec, "sqlite", "manifest-saved")
	if err := os.MkdirAll(filepath.Dir(spec.DBPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.DBPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), spec, "sqlite"); err != nil {
		t.Fatalf("rerun over crashed target init: %v", err)
	}
	store := openSQLite(t, spec)
	if got := scanAll(t, store, "vms"); len(got) != 4 || got["records/id1"] != `{"v":1}` {
		t.Fatalf("content after rerun: %v", got)
	}
}

func testSpec(t *testing.T, nss ...string) Spec {
	t.Helper()
	root := t.TempDir()
	spec := Spec{
		MetaRoot:   filepath.Join(root, "meta"),
		DBPath:     filepath.Join(root, "meta", metasqlite.DBFileName),
		DaemonLock: filepath.Join(root, "locks", "daemon.lock"),
	}
	for _, ns := range nss {
		spec.Decls = append(spec.Decls, metasqlite.Namespace{Name: ns, Tables: testTables})
		spec.JSON = append(spec.JSON, metajson.Namespace{
			Name:     ns,
			FilePath: filepath.Join(root, ns+".json"),
			LockPath: filepath.Join(root, ns+".lock"),
			Codec:    metajson.GenericCodec{},
		})
	}
	return spec
}

func putRaw(t *testing.T, s meta.Store, ns, table, id, raw string) {
	t.Helper()
	err := s.Update(t.Context(), meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(t.Context(), ns, table, id, jsontext.Value(raw), false)
	})
	if err != nil {
		t.Fatalf("put %s/%s/%s: %v", ns, table, id, err)
	}
}

func openSQLite(t *testing.T, spec Spec) meta.Store {
	t.Helper()
	s, err := metasqlite.Open(spec.DBPath, spec.Decls...)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openJSON(t *testing.T, spec Spec) meta.Store {
	t.Helper()
	s, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatalf("open json: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func beginConversion(t *testing.T, spec Spec) meta.Store {
	t.Helper()
	src, err := openSource(spec, "sqlite")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	m, err := newManifest(t.Context(), spec, "sqlite", src)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := saveManifest(spec.MetaRoot, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	return src
}

func runCrashingAt(t *testing.T, spec Spec, target, step string) {
	t.Helper()
	errCrash := errors.New("injected crash")
	testCrashStep = func(at string) error {
		if at == step {
			return errCrash
		}
		return nil
	}
	err := Run(t.Context(), spec, target)
	testCrashStep = nil
	if !errors.Is(err, errCrash) {
		t.Fatalf("step %s: want injected crash, got %v", step, err)
	}
}

func seedJSON(t *testing.T, spec Spec, ns string) {
	t.Helper()
	s, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatalf("open json: %v", err)
	}
	defer s.Close()
	err = s.Update(t.Context(), meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
		for id, raw := range map[string]string{"id1": `{"v":1}`, "id2": `{"v":2}`} {
			if err := w.PutRaw(t.Context(), ns, "records", id, jsontext.Value(raw), false); err != nil {
				return err
			}
		}
		if err := w.PutRaw(t.Context(), ns, "names", "alpha", jsontext.Value(`"id1"`), false); err != nil {
			return err
		}
		return w.PutRaw(t.Context(), ns, "tombstones", "id9", jsontext.Value(`{"dead":true}`), false)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func scanAll(t *testing.T, s meta.Store, ns string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := s.View(t.Context(), []string{ns}, func(r meta.Reader) error {
		for _, tbl := range testTables {
			if err := r.ScanRaw(t.Context(), ns, tbl, func(id string, raw jsontext.Value) error {
				got[tbl+"/"+id] = string(raw)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", ns, err)
	}
	return got
}
