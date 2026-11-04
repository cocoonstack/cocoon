package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	diskRefSuffix = ".ref"

	seriesDirPrefix = captureDirPrefix + "migrate-"
)

// Migrator captures a running VM in rounds and restores the result on another node.
type Migrator interface {
	// MigrateCapture starts a new series when req.Series is empty and adds a round to it otherwise.
	MigrateCapture(ctx context.Context, ref string, req MigrateRequest) (*MigrateRound, error)
	// MigrateReceive restores a transferred series into a new VM and leaves its guest paused; srcDir stays read-only.
	MigrateReceive(ctx context.Context, vmID string, vmCfg *types.VMConfig, net types.NetSetup, snapshotConfig *types.SnapshotConfig, srcDir string) (*types.VM, error)
	// MigrateAbort ends the series, resumes the guest and deletes the series directory.
	MigrateAbort(ctx context.Context, ref, series string) error
}

// MigrateRequest selects the round to capture; Final leaves the guest paused.
type MigrateRequest struct {
	Series string
	Final  bool
}

// MigrateRound reports one captured round; round 0 is the baseline and Files lists what the round wrote.
type MigrateRound struct {
	Series string        `json:"series"`
	Dir    string        `json:"dir"`
	Round  int           `json:"round"`
	Paused bool          `json:"paused"`
	Files  []MigrateFile `json:"files"`
}

type MigrateFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// MigrateSpec carries backend hooks for the migrate sequences.
type MigrateSpec struct {
	SnapshotSpec
	EndSeries TransitionHook
	Rounds    func(dir string) (int, error)
}

type roundSteps struct {
	series     string
	round      int
	final      bool
	before     func(hc *http.Client) error
	inPause    func() error
	afterPause func() error
}

// MigrateCaptureSequence runs one round; any failure ends the series, resumes the guest and deletes the directory.
func (b *Backend) MigrateCaptureSequence(ctx context.Context, ref string, req MigrateRequest, spec MigrateSpec) (*MigrateRound, error) {
	vmID, rec, unlock, err := b.lockForCapture(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var round *MigrateRound
	if req.Series == "" {
		round, err = b.captureBaseline(ctx, &rec, spec)
	} else {
		round, err = b.captureDelta(ctx, &rec, req, spec)
	}
	if err != nil {
		return nil, fmt.Errorf("migrate capture VM %s: %w", vmID, err)
	}
	return round, nil
}

// MigrateAbortSequence acts only while the series directory exists.
func (b *Backend) MigrateAbortSequence(ctx context.Context, ref, series string, spec MigrateSpec) error {
	vmID, rec, unlock, err := b.lockForCapture(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := seriesDir(&rec, series)
	if err != nil {
		return err
	}
	if !utils.FileExists(dir) {
		return nil
	}
	hc := utils.NewSocketHTTPClient(SocketPath(rec.RunDir))
	runErr := b.WithRunningVM(ctx, &rec, func(_ int) error {
		return errors.Join(spec.EndSeries(&rec, hc), resumeAfterCapture(spec.Resume, &rec, hc))
	})
	if runErr != nil && !errors.Is(runErr, ErrNotRunning) {
		return fmt.Errorf("migrate abort VM %s: %w", vmID, runErr)
	}
	return os.RemoveAll(dir)
}

func (b *Backend) captureBaseline(ctx context.Context, rec *VMRecord, spec MigrateSpec) (*MigrateRound, error) {
	// A fallback copy of the disks would run inside the pause and share no extents with the live disk.
	if err := utils.CheckReflink(rec.RunDir); err != nil {
		return nil, fmt.Errorf("live migration needs a reflink filesystem under the run dir: %w", err)
	}
	if errs := utils.RemoveMatching(ctx, rec.RunDir, isSeriesDir); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	dir, err := os.MkdirTemp(rec.RunDir, seriesDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("create series dir: %w", err)
	}
	series := strings.TrimPrefix(filepath.Base(dir), seriesDirPrefix)
	return b.captureRound(ctx, rec, spec, dir, roundSteps{
		series: series,
		before: func(hc *http.Client) error { return spec.EndSeries(rec, hc) },
		inPause: func() error {
			return CopyWritableDisks(ctx, dir, rec)
		},
		afterPause: func() error {
			if err := saveCaptureMeta(rec, spec.SnapshotSpec, dir); err != nil {
				return err
			}
			return b.writeSeriesEnvelope(rec, series, dir)
		},
	})
}

func (b *Backend) captureDelta(ctx context.Context, rec *VMRecord, req MigrateRequest, spec MigrateSpec) (*MigrateRound, error) {
	dir, err := seriesDir(rec, req.Series)
	if err != nil {
		return nil, err
	}
	pairs, err := writableDiskPairs(dir, rec)
	if err != nil {
		return nil, err
	}
	number := nextRound(pairs[0][0])
	steps := roundSteps{
		series: req.Series,
		round:  number,
		final:  req.Final,
		before: func(*http.Client) error {
			if err := checkRoundsInStep(spec, dir, pairs, number); err != nil {
				return err
			}
			return b.writeSeriesEnvelope(rec, req.Series, dir)
		},
		inPause: func() error {
			if req.Final {
				return nil
			}
			return reflinkNextRefs(ctx, pairs)
		},
		afterPause: func() error {
			_, mapErr := utils.Map(ctx, pairs, func(_ context.Context, _ int, p [2]string) (struct{}, error) {
				return struct{}{}, writeRoundDelta(p, number, req.Final)
			})
			return mapErr
		},
	}
	return b.captureRound(ctx, rec, spec, dir, steps)
}

func (b *Backend) captureRound(ctx context.Context, rec *VMRecord, spec MigrateSpec, dir string, steps roundSteps) (*MigrateRound, error) {
	before, err := stampFiles(dir)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	hc := utils.NewSocketHTTPClient(SocketPath(rec.RunDir))
	run := func() error {
		if beforeErr := steps.before(hc); beforeErr != nil {
			return beforeErr
		}
		if pauseErr := spec.Pause(rec, hc); pauseErr != nil {
			return fmt.Errorf("pause: %w", pauseErr)
		}
		if captureErr := spec.Capture(rec, hc, dir); captureErr != nil {
			return captureErr
		}
		if stepErr := steps.inPause(); stepErr != nil {
			return stepErr
		}
		if !steps.final {
			if resumeErr := resumeAfterCapture(spec.Resume, rec, hc); resumeErr != nil {
				return resumeErr
			}
		}
		return steps.afterPause()
	}
	if err = b.WithRunningVM(ctx, rec, func(_ int) error {
		if runErr := run(); runErr != nil {
			return errors.Join(runErr, spec.EndSeries(rec, hc), resumeAfterCapture(spec.Resume, rec, hc))
		}
		return nil
	}); err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	files, err := changedFiles(dir, before)
	if err != nil {
		return nil, err
	}
	return &MigrateRound{Series: steps.series, Dir: dir, Round: steps.round, Paused: steps.final, Files: files}, nil
}

func (b *Backend) writeSeriesEnvelope(rec *VMRecord, series, dir string) error {
	return snapshot.WriteSnapshotEnvelope(dir, *b.BuildSnapshotConfig(series, rec), nil, nil)
}

// IsMigrateDeltaFile reports a disk delta or reference a clone of a series must leave behind.
func IsMigrateDeltaFile(name string) bool {
	return strings.HasSuffix(name, diskRefSuffix) || strings.Contains(name, diskDeltaInfix)
}

func seriesDir(rec *VMRecord, series string) (string, error) {
	if !filepath.IsLocal(series) || strings.ContainsRune(series, filepath.Separator) {
		return "", fmt.Errorf("invalid series id %q", series)
	}
	return filepath.Join(rec.RunDir, seriesDirPrefix+series), nil
}

func isSeriesDir(e os.DirEntry) bool {
	return e.IsDir() && strings.HasPrefix(e.Name(), seriesDirPrefix)
}

func checkRoundsInStep(spec MigrateSpec, dir string, pairs [][2]string, round int) error {
	rounds, err := spec.Rounds(dir)
	if err != nil {
		return err
	}
	if rounds != round-1 {
		return fmt.Errorf("series holds %d guest rounds and %d disk rounds: an earlier capture died, start a new series", rounds, round-1)
	}
	for _, p := range pairs {
		if n := nextRound(p[0]); n != round {
			return fmt.Errorf("%s is at round %d, want %d: an earlier capture died, start a new series", filepath.Base(p[0]), n-1, round-1)
		}
	}
	return nil
}

func nextRound(disk string) int {
	n := 1
	for utils.FileExists(filepath.Join(filepath.Dir(disk), diskDeltaName(filepath.Base(disk), n))) {
		n++
	}
	return n
}

// reflinkNextRefs freezes each disk in the pause window so the delta can be written after the guest resumes.
func reflinkNextRefs(ctx context.Context, pairs [][2]string) error {
	refs := make([][2]string, len(pairs))
	for i, p := range pairs {
		refs[i] = [2]string{nextRefPath(p[0]), p[1]}
	}
	return copyPairs(ctx, refs)
}

func writeRoundDelta(pair [2]string, round int, final bool) error {
	copyPath, live := pair[0], pair[1]
	ref := copyPath + diskRefSuffix
	if !utils.FileExists(ref) {
		ref = copyPath
	}
	dst := filepath.Join(filepath.Dir(copyPath), diskDeltaName(filepath.Base(copyPath), round))
	if final {
		return writeDiskDelta(dst, ref, live)
	}
	next := nextRefPath(copyPath)
	if err := writeDiskDelta(dst, ref, next); err != nil {
		return err
	}
	return os.Rename(next, copyPath+diskRefSuffix)
}

func nextRefPath(copyPath string) string {
	return copyPath + ".next" + diskRefSuffix
}

func stampFiles(dir string) (map[string]os.FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read series dir: %w", err)
	}
	stamps := make(map[string]os.FileInfo, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		if info.Mode().IsRegular() && !strings.HasSuffix(e.Name(), diskRefSuffix) {
			stamps[e.Name()] = info
		}
	}
	return stamps, nil
}

func changedFiles(dir string, before map[string]os.FileInfo) ([]MigrateFile, error) {
	after, err := stampFiles(dir)
	if err != nil {
		return nil, err
	}
	files := []MigrateFile{}
	for _, name := range slices.Sorted(maps.Keys(after)) {
		info := after[name]
		if old, ok := before[name]; !ok || old.Size() != info.Size() || !old.ModTime().Equal(info.ModTime()) {
			files = append(files, MigrateFile{Name: name, Size: info.Size()})
		}
	}
	return files, nil
}
