package convert

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/utils"
)

var testTables = []string{"records", "names", "tombstones"}

func testSpec(t *testing.T, nss ...string) Spec {
	t.Helper()
	root := t.TempDir()
	spec := Spec{MetaRoot: filepath.Join(root, "meta"), DBPath: filepath.Join(root, "meta", metasqlite.DBFileName)}
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

func seedJSON(t *testing.T, spec Spec, ns string) {
	t.Helper()
	s, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatalf("open json: %v", err)
	}
	defer s.Close() //nolint:errcheck
	err = s.Update(t.Context(), meta.Scope{Write: ns}, meta.CommitDurable, func(w meta.Writer) error {
		for id, raw := range map[string]string{"id1": `{"v":1}`, "id2": `{"v":2}`} {
			if err := w.PutRaw(t.Context(), ns, "records", id, json.RawMessage(raw), false); err != nil {
				return err
			}
		}
		if err := w.PutRaw(t.Context(), ns, "names", "alpha", json.RawMessage(`"id1"`), false); err != nil {
			return err
		}
		return w.PutRaw(t.Context(), ns, "tombstones", "id9", json.RawMessage(`{"dead":true}`), false)
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
			if err := r.ScanRaw(t.Context(), ns, tbl, func(id string, raw json.RawMessage) error {
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
	sq, err := metasqlite.Open(spec.DBPath, spec.Decls...)
	if err != nil {
		t.Fatalf("open sqlite after convert: %v", err)
	}
	if got := scanAll(t, sq, "vms"); len(got) != 4 || got["records/id1"] != `{"v":1}` || got["names/alpha"] != `"id1"` {
		t.Fatalf("sqlite content: %v", got)
	}
	err = sq.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "id3", json.RawMessage(`{"v":3}`), false)
	})
	if err != nil {
		t.Fatalf("write in sqlite: %v", err)
	}
	if err := sq.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	if err := Run(ctx, spec, "json"); err != nil {
		t.Fatalf("convert back to json: %v", err)
	}
	if utils.FileExists(spec.DBPath) {
		t.Fatal("sqlite source not retired aside")
	}
	js, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatalf("open json after convert: %v", err)
	}
	defer js.Close() //nolint:errcheck
	got := scanAll(t, js, "vms")
	if len(got) != 5 || got["records/id3"] != `{"v":3}` || got["tombstones/id9"] != `{"dead":true}` {
		t.Fatalf("json content after round trip: %v", got)
	}
}

func TestDanglingNameRefused(t *testing.T) {
	spec := testSpec(t, "vms")
	s, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatalf("open json: %v", err)
	}
	err = s.Update(t.Context(), meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(t.Context(), "vms", "names", "ghost", json.RawMessage(`"missing"`), false)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = s.Close()
	err = Run(t.Context(), spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "missing record") {
		t.Fatalf("want dangling-name refusal, got %v", err)
	}
}

func TestResumeClaimsCommittedTarget(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	// Crash window: target fully committed, manifest not yet marked done.
	src, err := openSource(spec, "sqlite")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	m, err := newManifest(ctx, spec, "sqlite", src)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := saveManifest(spec.MetaRoot, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	dst, err := openTarget(ctx, spec, "sqlite")
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if err := copyNamespace(ctx, src, dst, spec.Decls[0]); err != nil {
		t.Fatalf("copy: %v", err)
	}
	_ = src.Close()
	_ = dst.Close()

	if err := Run(ctx, spec, "sqlite"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	sq, err := metasqlite.Open(spec.DBPath, spec.Decls...)
	if err != nil {
		t.Fatalf("open after resume: %v", err)
	}
	defer sq.Close() //nolint:errcheck
	if got := scanAll(t, sq, "vms"); len(got) != 4 {
		t.Fatalf("content after resume: %v", got)
	}
}

func TestSourceChangedRefused(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	src, err := openSource(spec, "sqlite")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	m, err := newManifest(ctx, spec, "sqlite", src)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := saveManifest(spec.MetaRoot, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	err = src.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "late", json.RawMessage(`{}`), false)
	})
	if err != nil {
		t.Fatalf("mutate source: %v", err)
	}
	_ = src.Close()

	err = Run(ctx, spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("want source-changed refusal, got %v", err)
	}
}

func TestTargetNotFreshRefused(t *testing.T) {
	ctx := t.Context()
	spec := testSpec(t, "vms")
	seedJSON(t, spec, "vms")

	if err := metasqlite.Init(ctx, spec.DBPath, spec.Decls...); err != nil {
		t.Fatalf("init: %v", err)
	}
	sq, err := metasqlite.Open(spec.DBPath, spec.Decls...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = sq.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "foreign", json.RawMessage(`{}`), false)
	})
	if err != nil {
		t.Fatalf("pollute target: %v", err)
	}
	_ = sq.Close()

	err = Run(ctx, spec, "sqlite")
	if err == nil || !strings.Contains(err.Error(), "target was not fresh") {
		t.Fatalf("want not-fresh refusal, got %v", err)
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
}

func TestCrashRerunMatrix(t *testing.T) {
	errCrash := errors.New("injected crash")
	forward := []string{"manifest-saved", "target-opened", "ns-copied", "ns-marked", "ns-done", "source-renamed"}
	reverse := []string{"manifest-saved", "target-opened", "ns-copied", "ns-done", "checkpointed", "source-renamed"}

	run := func(t *testing.T, target string, steps []string) {
		for _, step := range steps {
			t.Run(target+"/"+step, func(t *testing.T) {
				spec := testSpec(t, "vms")
				seedJSON(t, spec, "vms")
				if target == "json" {
					// Reverse direction starts from a sqlite store.
					if err := Run(t.Context(), spec, "sqlite"); err != nil {
						t.Fatalf("forward convert: %v", err)
					}
				}
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
				if rerr := Run(t.Context(), spec, target); rerr != nil {
					t.Fatalf("rerun after crash at %s: %v", step, rerr)
				}
				var store meta.Store
				var oerr error
				if target == "sqlite" {
					store, oerr = metasqlite.Open(spec.DBPath, spec.Decls...)
				} else {
					store, oerr = metajson.Open(spec.JSON...)
				}
				if oerr != nil {
					t.Fatalf("open target after rerun: %v", oerr)
				}
				defer store.Close() //nolint:errcheck
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
	// Two committed generations: main and .prev are DISTINCT (§9).
	js, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatal(err)
	}
	for gen := 1; gen <= 2; gen++ {
		err = js.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
			return w.PutRaw(ctx, "vms", "records", "id1", json.RawMessage(`{"gen":`+strconv.Itoa(gen)+`}`), false)
		})
		if err != nil {
			t.Fatal(err)
		}
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

	// Corrupt the new main: the served generation must be the imported one,
	// never a stranded pre-conversion .prev (§9).
	main, err := os.ReadFile(spec.JSON[0].FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.JSON[0].FilePath, append(main, []byte("garbage")...), 0o644); err != nil {
		t.Fatal(err)
	}
	js2, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatal(err)
	}
	defer js2.Close() //nolint:errcheck
	if got := scanAll(t, js2, "vms"); got["records/id1"] != `{"gen":2}` {
		t.Fatalf("served generation after torn main: %v", got)
	}
}

// TestUncheckpointedWALReverseConversion: a commit stranded in the WAL by a
// killed process must survive the checkpoint-then-aside reverse conversion (§9).
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

	cmd := exec.Command(os.Args[0], "-test.run=TestWALWriterWorker$", "-test.count=1") //nolint:gosec
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
	js, err := metajson.Open(spec.JSON...)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close() //nolint:errcheck
	if got := scanAll(t, js, "vms"); got["records/walrec"] != `{"wal":1}` {
		t.Fatalf("WAL-stranded commit lost across reverse conversion: %v", got)
	}
}

// TestWALWriterWorker commits one durable record, acks, and hangs until killed
// so the WAL is never checkpointed by a clean close.
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
	ctx := t.Context()
	err = s.Update(ctx, meta.Scope{Write: "vms"}, meta.CommitDurable, func(w meta.Writer) error {
		return w.PutRaw(ctx, "vms", "records", "walrec", json.RawMessage(`{"wal":1}`), false)
	})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ACK")
	select {} //nolint:staticcheck // hang until SIGKILL keeps the WAL un-checkpointed
}

// TestConvertSkipsNeverWrittenNamespace: a namespace whose lock dir was
// never created (subsystem never ran) must not fail the quiesce probe.
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
	sq, err := metasqlite.Open(spec.DBPath, spec.Decls...)
	if err != nil {
		t.Fatalf("open after convert: %v", err)
	}
	defer sq.Close() //nolint:errcheck
	if got := scanAll(t, sq, "vms"); len(got) != 4 {
		t.Fatalf("content: %v", got)
	}
	_ = sq.Close()
	if err := Run(t.Context(), spec, "json"); err != nil {
		t.Fatalf("reverse convert with never-written namespace: %v", err)
	}
}
