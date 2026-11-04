package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/snapshot"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const diskRefSuffix = ".ref"

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

// MigrateFile is one file a round wrote.
type MigrateFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// MigrateSpec carries backend hooks for the migrate sequences; SnapshotSpec.Capture writes the guest state of one round.
type MigrateSpec struct {
	SnapshotSpec
	EndSeries TransitionHook
}

// roundSteps is what a baseline and a delta do differently around the shared pause window; round 0 is the baseline.
type roundSteps struct {
	series     string
	round      int
	final      bool
	inPause    func() error
	afterPause func() error
}

type fileStamp struct {
	size    int64
	modTime int64
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

// MigrateAbortSequence is idempotent; a VM that no longer runs only loses the directory.
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
	hc := utils.NewSocketHTTPClient(SocketPath(rec.RunDir))
	runErr := b.WithRunningVM(ctx, &rec, func(_ int) error {
		return errors.Join(spec.EndSeries(&rec, hc), resumeAfterCapture(spec.Resume, &rec, hc))
	})
	if errors.Is(runErr, ErrNotRunning) {
		runErr = nil
	}
	if runErr = errors.Join(runErr, os.RemoveAll(dir)); runErr != nil {
		return fmt.Errorf("migrate abort VM %s: %w", vmID, runErr)
	}
	return nil
}

func (b *Backend) captureBaseline(ctx context.Context, rec *VMRecord, spec MigrateSpec) (*MigrateRound, error) {
	// A fallback copy of the disks would run inside the pause and share no extents with the live disk.
	if err := utils.CheckReflink(rec.RunDir); err != nil {
		return nil, fmt.Errorf("live migration needs a reflink filesystem under the run dir: %w", err)
	}
	dir, err := os.MkdirTemp(rec.RunDir, captureDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("create series dir: %w", err)
	}
	series := strings.TrimPrefix(filepath.Base(dir), captureDirPrefix)
	return b.captureRound(ctx, rec, spec, dir, roundSteps{
		series: series,
		inPause: func() error {
			return CopyWritableDisks(ctx, dir, rec)
		},
		afterPause: func() error {
			if err := saveCaptureMeta(rec, spec.SnapshotSpec, dir); err != nil {
				return err
			}
			return snapshot.WriteSnapshotEnvelope(dir, *b.BuildSnapshotConfig(series, rec), nil, nil)
		},
	})
}

func (b *Backend) captureDelta(ctx context.Context, rec *VMRecord, req MigrateRequest, spec MigrateSpec) (*MigrateRound, error) {
	dir, err := seriesDir(rec, req.Series)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dir); err != nil {
		return nil, fmt.Errorf("series %s: %w", req.Series, err)
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
		if steps.round == 0 {
			if endErr := spec.EndSeries(rec, hc); endErr != nil {
				return fmt.Errorf("end previous series: %w", endErr)
			}
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

// IsMigrateDeltaFile reports a disk delta or reference a clone of a series must leave behind.
func IsMigrateDeltaFile(name string) bool {
	return strings.HasSuffix(name, diskRefSuffix) || strings.Contains(name, diskDeltaInfix)
}

func seriesDir(rec *VMRecord, series string) (string, error) {
	if series == "" || !filepath.IsLocal(series) || strings.ContainsRune(series, filepath.Separator) {
		return "", fmt.Errorf("invalid series id %q", series)
	}
	return filepath.Join(rec.RunDir, captureDirPrefix+series), nil
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

func stampFiles(dir string) (map[string]fileStamp, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read series dir: %w", err)
	}
	stamps := make(map[string]fileStamp, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		if info.Mode().IsRegular() && !strings.HasSuffix(e.Name(), diskRefSuffix) {
			stamps[e.Name()] = fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
		}
	}
	return stamps, nil
}

func changedFiles(dir string, before map[string]fileStamp) ([]MigrateFile, error) {
	after, err := stampFiles(dir)
	if err != nil {
		return nil, err
	}
	files := []MigrateFile{}
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if stamp, ok := before[name]; !ok || stamp != after[name] {
			files = append(files, MigrateFile{Name: name, Size: after[name].size})
		}
	}
	return files, nil
}
