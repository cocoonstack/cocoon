// Package convert is the explicit offline engine cutover (design §6): a
// standalone fsync-first manifest is the only recovery authority, targets
// are fresh, sources retire aside, and every step is crash-rerunnable.
package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	asideSuffix = ".converted-"

	// EngineJSON/EngineSQLite are the meta_backend values.
	EngineJSON   = "json"
	EngineSQLite = "sqlite"
)

// testCrashStep injects crashes between protocol steps (§9 gate); nil in prod.
var testCrashStep func(step string) error

func crashStep(step string) error {
	if testCrashStep == nil {
		return nil
	}
	return testCrashStep(step)
}

// Spec carries both engines' declarations from the composition root.
type Spec struct {
	MetaRoot string // manifest directory (parent of the sqlite DB)
	DBPath   string
	Decls    []metasqlite.Namespace
	JSON     []metajson.Namespace
}

// Manifest is the crash-recovery authority for one conversion run.
type Manifest struct {
	Target     string               `json:"target"`
	Namespaces map[string]*NSRecord `json:"namespaces"`
	StartedAt  time.Time            `json:"started_at"`
}

// NSRecord pins one namespace's source identity and completion.
type NSRecord struct {
	Files   []string `json:"files"` // json source: main+.prev; sqlite source: db path
	SHA256  string   `json:"sha256"`
	Records int      `json:"records"`
	Done    bool     `json:"done"`
}

// Run performs (or resumes) the cutover to target ("sqlite" or "json").
func Run(ctx context.Context, spec Spec, target string) error {
	if target != EngineSQLite && target != EngineJSON {
		return fmt.Errorf("unknown target engine %q", target)
	}
	m, err := loadManifest(spec.MetaRoot)
	if err != nil {
		return err
	}
	if m != nil && m.Target != target {
		return fmt.Errorf("conversion to %q already in flight; finish it first", m.Target)
	}
	if m == nil || !allDone(m) {
		if err := convertAll(ctx, spec, target, m); err != nil {
			return err
		}
	}
	if err := retireSources(ctx, spec, target); err != nil {
		return err
	}
	return finishManifest(spec.MetaRoot)
}

func convertAll(ctx context.Context, spec Spec, target string, m *Manifest) error {
	src, err := openSource(spec, target)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck
	if qerr := checkQuiesced(ctx, spec, target, src); qerr != nil {
		return qerr
	}
	if m == nil {
		if m, err = newManifest(ctx, spec, target, src); err != nil {
			return err
		}
		// The manifest is durable before the first target byte (§6).
		if serr := saveManifest(spec.MetaRoot, m); serr != nil {
			return serr
		}
	}
	if cerr := crashStep("manifest-saved"); cerr != nil {
		return cerr
	}
	dst, err := openTarget(ctx, spec, target)
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck
	if err := crashStep("target-opened"); err != nil {
		return err
	}
	for _, ns := range spec.Decls {
		rec := m.Namespaces[ns.Name]
		if rec == nil {
			return fmt.Errorf("namespace %s missing from the manifest; it predates this declaration", ns.Name)
		}
		if rec.Done {
			continue
		}
		if err := convertNamespace(ctx, src, dst, ns, rec, m, spec); err != nil {
			return fmt.Errorf("namespace %s: %w", ns.Name, err)
		}
	}
	return nil
}

func newManifest(ctx context.Context, spec Spec, target string, src meta.Store) (*Manifest, error) {
	m := &Manifest{Target: target, Namespaces: map[string]*NSRecord{}, StartedAt: time.Now()}
	for _, ns := range spec.Decls {
		digest, count, err := canonicalDigest(ctx, src, ns)
		if err != nil {
			return nil, err
		}
		m.Namespaces[ns.Name] = &NSRecord{Files: sourceFiles(spec, target, ns.Name), SHA256: digest, Records: count}
	}
	return m, nil
}

// checkQuiesced is §6's advisory activity check: an in-flight source write
// means live cocoon processes, and converting under them could lose writes
// landing on a namespace already marked done.
func checkQuiesced(ctx context.Context, spec Spec, target string, src meta.Store) error {
	if target == EngineJSON {
		// sqlite source: an empty durable transaction fails ErrBusy while a
		// writer is mid-flight.
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		err := src.Update(probeCtx, meta.Scope{Write: spec.Decls[0].Name}, meta.CommitDurable, func(meta.Writer) error { return nil })
		if err != nil {
			return fmt.Errorf("sqlite store busy; stop cocoon activity before converting: %w", err)
		}
		return nil
	}
	for _, jns := range spec.JSON {
		// Persistent form: the engine's own locks are persistent, and a
		// transient probe would delete the shared lock file on release.
		l := flock.New(jns.LockPath)
		ok, err := l.TryLock(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("namespace %s lock is held; stop cocoon activity before converting", jns.Name)
		}
		if err := l.Unlock(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ensureJSONDirs creates namespace dirs for subsystems that never ran —
// their absence means empty, and the engine's flock needs the dir to exist.
func ensureJSONDirs(spec Spec) error {
	for _, jns := range spec.JSON {
		if err := os.MkdirAll(filepath.Dir(jns.LockPath), 0o750); err != nil {
			return err
		}
	}
	return nil
}

// openSource opens the engine being converted FROM (the opposite of target).
func openSource(spec Spec, target string) (meta.Store, error) {
	if target == EngineSQLite {
		if err := ensureJSONDirs(spec); err != nil {
			return nil, err
		}
		return metajson.Open(spec.JSON...)
	}
	// The driver would create an empty file on first touch; a missing source
	// must fail before that.
	if !utils.FileExists(spec.DBPath) {
		return nil, fmt.Errorf("no sqlite store at %s to convert from", spec.DBPath)
	}
	return metasqlite.OpenForRecovery(spec.DBPath, spec.Decls...)
}

func openTarget(ctx context.Context, spec Spec, target string) (meta.Store, error) {
	if target == EngineJSON {
		if err := ensureJSONDirs(spec); err != nil {
			return nil, err
		}
		return metajson.Open(spec.JSON...)
	}
	if !utils.FileExists(spec.DBPath) {
		if err := metasqlite.InitForRecovery(ctx, spec.DBPath, spec.Decls...); err != nil {
			return nil, err
		}
	}
	return metasqlite.OpenForRecovery(spec.DBPath, spec.Decls...)
}

func convertNamespace(ctx context.Context, src, dst meta.Store, ns metasqlite.Namespace, rec *NSRecord, m *Manifest, spec Spec) error {
	// A committed-but-unmarked target (crash window) is re-verified and
	// claimed, never redone (§6).
	dstDigest, dstCount, err := canonicalDigest(ctx, dst, ns)
	if err != nil {
		return err
	}
	if dstDigest != rec.SHA256 || dstCount != rec.Records {
		if err := copyAndVerify(ctx, src, dst, ns, rec); err != nil {
			return err
		}
	}
	if err := crashStep("ns-copied"); err != nil {
		return err
	}
	if m.Target == EngineSQLite {
		if err := metasqlite.MarkConverted(ctx, spec.DBPath, ns.Name, rec.Files[0], rec.SHA256, rec.Records); err != nil {
			return err
		}
		if err := crashStep("ns-marked"); err != nil {
			return err
		}
	} else if err := duplicateGeneration(spec, ns.Name); err != nil {
		return err
	}
	rec.Done = true
	if err := saveManifest(spec.MetaRoot, m); err != nil {
		return err
	}
	return crashStep("ns-done")
}

// duplicateGeneration copies a freshly written json target to its .prev so
// the imported data survives a later torn main (§9: never less resilient
// than steady state).
func duplicateGeneration(spec Spec, nsName string) error {
	jns, ok := findJSON(spec, nsName)
	if !ok {
		return nil
	}
	data, err := os.ReadFile(jns.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // empty namespace wrote no file
	}
	if err != nil {
		return err
	}
	return utils.AtomicWriteFile(jns.FilePath+".prev", data, 0o644)
}

func findJSON(spec Spec, nsName string) (metajson.Namespace, bool) {
	i := slices.IndexFunc(spec.JSON, func(j metajson.Namespace) bool { return j.Name == nsName })
	if i < 0 {
		return metajson.Namespace{}, false
	}
	return spec.JSON[i], true
}

func copyAndVerify(ctx context.Context, src, dst meta.Store, ns metasqlite.Namespace, rec *NSRecord) error {
	srcDigest, srcCount, err := canonicalDigest(ctx, src, ns)
	if err != nil {
		return err
	}
	if srcDigest != rec.SHA256 || srcCount != rec.Records {
		return fmt.Errorf("source changed since the manifest was written (records %d→%d); remove the manifest and restart", rec.Records, srcCount)
	}
	if verr := verifyNames(ctx, src, ns); verr != nil {
		return verr
	}
	if cerr := copyNamespace(ctx, src, dst, ns); cerr != nil {
		return cerr
	}
	gotDigest, gotCount, err := canonicalDigest(ctx, dst, ns)
	if err != nil {
		return err
	}
	if gotDigest != rec.SHA256 || gotCount != rec.Records {
		return fmt.Errorf("target verification failed (records %d, want %d); target was not fresh", gotCount, rec.Records)
	}
	return nil
}

func copyNamespace(ctx context.Context, src, dst meta.Store, ns metasqlite.Namespace) error {
	type row struct {
		table, id string
		raw       json.RawMessage
	}
	var rows []row
	if err := src.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		for _, tbl := range ns.Tables {
			if err := r.ScanRaw(ctx, ns.Name, tbl, func(id string, raw json.RawMessage) error {
				rows = append(rows, row{tbl, id, slices.Clone(raw)})
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// One durable transaction per namespace (§6).
	return dst.Update(ctx, meta.Scope{Write: ns.Name}, meta.CommitDurable, func(w meta.Writer) error {
		for _, r := range rows {
			if err := w.PutRaw(ctx, ns.Name, r.table, r.id, r.raw, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// verifyNames is §6's referential name check: every name entry must point at
// an existing record. Table names are the record-SPI convention shared by
// every namespace declaration.
func verifyNames(ctx context.Context, s meta.Store, ns metasqlite.Namespace) error {
	if !slices.Contains(ns.Tables, "names") {
		return nil
	}
	return s.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		return r.ScanRaw(ctx, ns.Name, "names", func(name string, raw json.RawMessage) error {
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				return fmt.Errorf("name %q: %w", name, err)
			}
			if _, ok, err := r.GetRaw(ctx, ns.Name, "records", id); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("name %q points at missing record %q", name, id)
			}
			return nil
		})
	})
}

// canonicalDigest is the engine-neutral namespace fingerprint (§6): rows
// sorted by id per table, tables in declaration order; count is records only.
func canonicalDigest(ctx context.Context, s meta.Store, ns metasqlite.Namespace) (string, int, error) {
	h := sha256.New()
	count := 0
	err := s.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		for _, tbl := range ns.Tables {
			collected := map[string]json.RawMessage{}
			if err := r.ScanRaw(ctx, ns.Name, tbl, func(id string, raw json.RawMessage) error {
				collected[id] = slices.Clone(raw)
				return nil
			}); err != nil {
				return err
			}
			for _, id := range slices.Sorted(maps.Keys(collected)) {
				_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x0a", tbl, id, collected[id])
				if tbl == "records" {
					count++
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), count, nil
}

// retireSources aside-renames every source file after full verification; a
// sqlite source checkpoints to a single file first (§6). Runs with both
// engines closed; already-renamed files are skipped, so a crash here reruns.
func retireSources(ctx context.Context, spec Spec, target string) error {
	if target == EngineJSON && utils.FileExists(spec.DBPath) {
		if err := metasqlite.Checkpoint(ctx, spec.DBPath); err != nil {
			return err
		}
		if err := crashStep("checkpointed"); err != nil {
			return err
		}
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	for _, ns := range spec.Decls {
		for _, f := range sourceFiles(spec, target, ns.Name) {
			if !utils.FileExists(f) {
				continue
			}
			if err := os.Rename(f, f+asideSuffix+ts); err != nil {
				return err
			}
			if err := utils.SyncParentDir(filepath.Dir(f)); err != nil {
				return err
			}
			if err := crashStep("source-renamed"); err != nil {
				return err
			}
		}
	}
	return nil
}

func sourceFiles(spec Spec, target, nsName string) []string {
	if target == EngineJSON {
		return []string{spec.DBPath}
	}
	if jns, ok := findJSON(spec, nsName); ok {
		// Both json generations are authority (§6).
		return []string{jns.FilePath, jns.FilePath + ".prev"}
	}
	return nil
}

func allDone(m *Manifest) bool {
	for _, rec := range m.Namespaces {
		if !rec.Done {
			return false
		}
	}
	return true
}

func manifestPath(root string) string {
	return filepath.Join(root, metasqlite.ManifestName)
}

func loadManifest(root string) (*Manifest, error) {
	raw, err := os.ReadFile(manifestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest corrupt: %w", err)
	}
	return &m, nil
}

func saveManifest(root string, m *Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	return utils.AtomicWriteFile(manifestPath(root), raw, 0o644)
}

func finishManifest(root string) error {
	if err := os.Remove(manifestPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return utils.SyncParentDir(root)
}
