package convert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
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
	if exists(spec.JSON[0].FilePath) {
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
	if exists(spec.DBPath) {
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
	dst, err := openTarget(spec, "sqlite")
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

	if err := metasqlite.Init(spec.DBPath, spec.Decls...); err != nil {
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
	if err := metasqlite.Init(spec.DBPath, spec.Decls...); err == nil || !strings.Contains(err.Error(), "conversion is in flight") {
		t.Fatalf("want init refusal, got %v", err)
	}
}
