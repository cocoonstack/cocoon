// Package convert runs the offline meta engine cutover.
package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/lock/flock"
	"github.com/cocoonstack/cocoon/meta"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	metasqlite "github.com/cocoonstack/cocoon/meta/sqlite"
	"github.com/cocoonstack/cocoon/utils"
)

const asideSuffix = ".converted-"

// testCrashStep injects crashes between protocol steps; nil in prod.
var testCrashStep func(step string) error

// Spec carries both engines' declarations from the composition root.
type Spec struct {
	MetaRoot   string // manifest directory (parent of the sqlite DB)
	DBPath     string
	DaemonLock string
	Decls      []metasqlite.Namespace
	JSON       []metajson.Namespace
}

// manifest is the crash-recovery authority for one conversion run.
type manifest struct {
	Target     string               `json:"target"`
	Namespaces map[string]*nsRecord `json:"namespaces"`
	StartedAt  time.Time            `json:"started_at"`
}

// nsRecord pins one namespace's source identity and completion.
type nsRecord struct {
	Files   []string `json:"files"` // json source: main+.prev; sqlite source: db path
	SHA256  string   `json:"sha256"`
	Records int      `json:"records"`
	Done    bool     `json:"done"`
}

// Run performs (or resumes) the cutover to target ("sqlite" or "json").
func Run(ctx context.Context, spec Spec, target string) error {
	if target != config.MetaBackendSQLite && target != config.MetaBackendJSON {
		return fmt.Errorf("unknown target engine %q", target)
	}
	unlock, err := lockStore(ctx, spec)
	if err != nil {
		return err
	}
	defer unlock()
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

// lockStore takes the lock every store opener shares, then the instance lock, which is all a daemon from an older binary holds.
func lockStore(ctx context.Context, spec Spec) (func(), error) {
	if err := utils.EnsureDirs(spec.MetaRoot, filepath.Dir(spec.DaemonLock)); err != nil {
		return nil, err
	}
	var held []*flock.Lock
	unlock := func() {
		for _, l := range held {
			_ = l.Unlock(ctx)
		}
	}
	for _, path := range []string{filepath.Join(spec.MetaRoot, metasqlite.StoreLockName), spec.DaemonLock} {
		l := flock.New(path)
		ok, err := l.TryLock(ctx)
		if err == nil && !ok {
			err = errors.New("cocoon processes hold the meta store; stop the daemon, any gc timer and running commands")
		}
		if err != nil {
			unlock()
			return nil, err
		}
		held = append(held, l)
	}
	return unlock, nil
}

func crashStep(step string) error {
	if testCrashStep == nil {
		return nil
	}
	return testCrashStep(step)
}

func convertAll(ctx context.Context, spec Spec, target string, m *manifest) error {
	if err := ensureJSONDirs(spec); err != nil {
		return err
	}
	src, err := openSource(spec, target)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck
	if m == nil {
		if ferr := checkFreshTarget(spec, target); ferr != nil {
			return ferr
		}
		if m, err = newManifest(ctx, spec, target, src); err != nil {
			return err
		}
		// The manifest is durable before the first target byte.
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

func checkFreshTarget(spec Spec, target string) error {
	if target == config.MetaBackendJSON {
		if jsonPresent(spec) {
			return errors.New("json target already exists; move its namespace files aside before converting")
		}
		return nil
	}
	if utils.FileExists(spec.DBPath) {
		return fmt.Errorf("%s already exists; move it aside with its -wal and -shm before converting", spec.DBPath)
	}
	if err := utils.EnsureDirs(filepath.Dir(spec.DBPath)); err != nil {
		return err
	}
	return metasqlite.CheckFS(spec.DBPath)
}

func newManifest(ctx context.Context, spec Spec, target string, src meta.Store) (*manifest, error) {
	m := &manifest{Target: target, Namespaces: map[string]*nsRecord{}, StartedAt: time.Now()}
	for _, ns := range spec.Decls {
		digest, count, err := canonicalDigest(ctx, src, ns)
		if err != nil {
			return nil, err
		}
		m.Namespaces[ns.Name] = &nsRecord{Files: sourceFiles(spec, target, ns.Name), SHA256: digest, Records: count}
	}
	return m, nil
}

// ensureJSONDirs creates namespace dirs for subsystems that never ran; flock needs them.
func ensureJSONDirs(spec Spec) error {
	dirs := make([]string, 0, len(spec.JSON))
	for _, jns := range spec.JSON {
		dirs = append(dirs, filepath.Dir(jns.LockPath))
	}
	return utils.EnsureDirs(dirs...)
}

// openSource opens the engine being converted FROM (the opposite of target).
func openSource(spec Spec, target string) (meta.Store, error) {
	if target == config.MetaBackendSQLite {
		if !jsonPresent(spec) {
			return nil, fmt.Errorf("no json store under %s to convert from", spec.MetaRoot)
		}
		return metajson.Open(spec.JSON...)
	}
	// The driver would create an empty file on first touch; a missing source must fail before that.
	if !utils.FileExists(spec.DBPath) {
		return nil, fmt.Errorf("no sqlite store at %s to convert from", spec.DBPath)
	}
	return metasqlite.OpenForRecovery(spec.DBPath, spec.Decls...)
}

func openTarget(ctx context.Context, spec Spec, target string) (meta.Store, error) {
	if target == config.MetaBackendJSON {
		return metajson.Open(spec.JSON...)
	}
	if err := metasqlite.InitForRecoveryIfNeeded(ctx, spec.DBPath, spec.Decls...); err != nil {
		return nil, err
	}
	return metasqlite.OpenForRecovery(spec.DBPath, spec.Decls...)
}

func convertNamespace(ctx context.Context, src, dst meta.Store, ns metasqlite.Namespace, rec *nsRecord, m *manifest, spec Spec) error {
	// A committed-but-unmarked target (crash window) is re-verified and claimed, never redone.
	dstDigest, dstCount, err := canonicalDigest(ctx, dst, ns)
	if err != nil {
		return err
	}
	if dstDigest != rec.SHA256 || dstCount != rec.Records {
		if err := copyAndVerify(ctx, src, dst, ns, rec, partialTarget(spec, m.Target)); err != nil {
			return err
		}
	}
	if err := crashStep("ns-copied"); err != nil {
		return err
	}
	if m.Target == config.MetaBackendSQLite {
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

// duplicateGeneration writes the json target's .prev so imported data survives a torn main.
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
	return utils.AtomicWriteFile(jns.FilePath+".prev", data, 0o644, utils.Sync)
}

func jsonPresent(spec Spec) bool {
	return slices.ContainsFunc(spec.JSON, func(ns metajson.Namespace) bool { return utils.FileExists(ns.FilePath) })
}

func findJSON(spec Spec, nsName string) (metajson.Namespace, bool) {
	i := slices.IndexFunc(spec.JSON, func(j metajson.Namespace) bool { return j.Name == nsName })
	if i < 0 {
		return metajson.Namespace{}, false
	}
	return spec.JSON[i], true
}

func partialTarget(spec Spec, target string) string {
	if target == config.MetaBackendJSON {
		return "the json namespace files it wrote"
	}
	return spec.DBPath + " with its -wal and -shm"
}

func copyAndVerify(ctx context.Context, src, dst meta.Store, ns metasqlite.Namespace, rec *nsRecord, partial string) error {
	srcDigest, srcCount, err := canonicalDigest(ctx, src, ns)
	if err != nil {
		return err
	}
	if srcDigest != rec.SHA256 || srcCount != rec.Records {
		return fmt.Errorf("source changed since the manifest was written (records %d→%d); remove the manifest and the partial target (%s), then restart", rec.Records, srcCount, partial)
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
		raw       jsontext.Value
	}
	var rows []row
	if err := src.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		for _, tbl := range ns.Tables {
			if err := r.ScanRaw(ctx, ns.Name, tbl, func(id string, raw jsontext.Value) error {
				rows = append(rows, row{tbl, id, raw})
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return dst.Update(ctx, meta.Scope{Write: ns.Name}, meta.CommitDurable, func(w meta.Writer) error {
		for _, r := range rows {
			if err := w.PutRaw(ctx, ns.Name, r.table, r.id, r.raw, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// verifyNames checks every name entry points at an existing record.
func verifyNames(ctx context.Context, s meta.Store, ns metasqlite.Namespace) error {
	if !slices.Contains(ns.Tables, "names") {
		return nil
	}
	return s.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		return r.ScanRaw(ctx, ns.Name, "names", func(name string, raw jsontext.Value) error {
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				return fmt.Errorf("name %q: %w", name, err)
			}
			_, ok, err := r.GetRaw(ctx, ns.Name, "records", id)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("name %q points at missing record %q", name, id)
			}
			return nil
		})
	})
}

// canonicalDigest is the engine-neutral namespace fingerprint: rows sorted by id per table, tables in declaration order; count is records only.
func canonicalDigest(ctx context.Context, s meta.Store, ns metasqlite.Namespace) (string, int, error) {
	h := sha256.New()
	count := 0
	err := s.View(ctx, []string{ns.Name}, func(r meta.Reader) error {
		for _, tbl := range ns.Tables {
			collected := map[string]jsontext.Value{}
			if err := r.ScanRaw(ctx, ns.Name, tbl, func(id string, raw jsontext.Value) error {
				collected[id] = raw
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

// retireSources renames verified source files aside; renamed files are skipped on rerun.
func retireSources(ctx context.Context, spec Spec, target string) error {
	if target == config.MetaBackendJSON && utils.FileExists(spec.DBPath) {
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
	if target == config.MetaBackendJSON {
		return []string{spec.DBPath}
	}
	if jns, ok := findJSON(spec, nsName); ok {
		// Both json generations are authority.
		return []string{jns.FilePath, jns.FilePath + ".prev"}
	}
	return nil
}

func allDone(m *manifest) bool {
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

func loadManifest(root string) (*manifest, error) {
	raw, err := os.ReadFile(manifestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest corrupt: %w", err)
	}
	return &m, nil
}

func saveManifest(root string, m *manifest) error {
	raw, err := json.Marshal(m, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	return utils.AtomicWriteFile(manifestPath(root), raw, 0o644, utils.Sync)
}

func finishManifest(root string) error {
	if err := os.Remove(manifestPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return utils.SyncParentDir(root)
}
