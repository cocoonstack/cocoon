package firecracker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/lock/vmlock"
	metajson "github.com/cocoonstack/cocoon/meta/json"
	"github.com/cocoonstack/cocoon/meta/tombstone"
	"github.com/cocoonstack/cocoon/types"
)

func TestRedirectedDriveIndices(t *testing.T) {
	sc := func(paths ...string) []*types.StorageConfig {
		configs := make([]*types.StorageConfig, len(paths))
		for i, p := range paths {
			configs[i] = &types.StorageConfig{Path: p}
		}
		return configs
	}
	tests := []struct {
		name     string
		src, dst []*types.StorageConfig
		want     []int
	}{
		{"all equal", sc("/a", "/b"), sc("/a", "/b"), nil},
		{"one differs", sc("/a", "/b", "/c"), sc("/a", "/B", "/c"), []int{1}},
		{"all differ", sc("/a", "/b"), sc("/A", "/B"), []int{0, 1}},
		{"dst shorter", sc("/a", "/b"), sc("/A"), []int{0}},
		{"empty", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redirectedDriveIndices(tt.src, tt.dst); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlaceholderCoalescesConcurrentRecordReads(t *testing.T) {
	rootDir := t.TempDir()
	runRoot := filepath.Join(rootDir, "run", "firecracker")
	if err := os.MkdirAll(runRoot, 0o750); err != nil {
		t.Fatalf("setup run root: %v", err)
	}
	src := []*types.StorageConfig{{Path: filepath.Join(runRoot, "gone", "cow.raw")}}
	dst := []*types.StorageConfig{{Path: writeTestFile(t, rootDir, "clone-cow.raw")}}
	var recordReads atomic.Int64
	lookup := func(string) (bool, error) {
		recordReads.Add(1)
		time.Sleep(20 * time.Millisecond)
		return false, nil
	}

	const clones = 64
	start := make(chan struct{})
	errs := make(chan error, clones)
	var wg sync.WaitGroup
	for range clones {
		wg.Go(func() {
			<-start
			plan, err := holdBindableRedirects(t.Context(), rootDir, runRoot, src, dst, lookup)
			if plan != nil {
				plan.close()
			}
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("hold bindable redirects: %v", err)
		}
	}
	if got := recordReads.Load(); got != 1 {
		t.Fatalf("source record reads = %d, want 1", got)
	}
}

func TestHoldBindableRedirectsCreatesPlaceholderAndSharesVMLock(t *testing.T) {
	rootDir := t.TempDir()
	runRoot := filepath.Join(rootDir, "run", "firecracker")
	if err := os.MkdirAll(runRoot, 0o750); err != nil {
		t.Fatalf("setup run root: %v", err)
	}
	gone := filepath.Join(runRoot, "gone")
	src := []*types.StorageConfig{
		{Path: filepath.Join(gone, "cow.raw")},
		{Path: filepath.Join(gone, "data-x.raw")},
	}
	dst := []*types.StorageConfig{
		{Path: writeTestFile(t, rootDir, "clone-cow.raw")},
		{Path: writeTestFile(t, rootDir, "clone-data-x.raw")},
	}

	recordReads := 0
	deadSource := func(string) (bool, error) {
		recordReads++
		return false, nil
	}
	plan1, err := holdBindableRedirects(t.Context(), rootDir, runRoot, src, dst, deadSource)
	if err != nil {
		t.Fatalf("first holdBindableRedirects: %v", err)
	}
	t.Cleanup(plan1.close)
	if len(plan1.binds) != 2 {
		t.Fatalf("bind count = %d, want 2", len(plan1.binds))
	}
	if recordReads != len(src) {
		t.Fatalf("source record reads = %d, want %d", recordReads, len(src))
	}
	for _, sc := range src {
		if fi, statErr := os.Lstat(sc.Path); statErr != nil || !fi.Mode().IsRegular() {
			t.Fatalf("placeholder %s: fi=%v err=%v", sc.Path, fi, statErr)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	plan2, err := holdBindableRedirects(ctx, rootDir, runRoot, src, dst, deadSource)
	if err != nil {
		t.Fatalf("second shared holdBindableRedirects: %v", err)
	}
	t.Cleanup(plan2.close)
	if recordReads != len(src) {
		t.Fatalf("existing placeholders did not suppress repeated record reads: %d", recordReads)
	}
	if len(plan2.files()) != 1 {
		t.Fatalf("inherited lease files = %d, want 1", len(plan2.files()))
	}
	gcLock, err := vmlock.New(rootDir, "gone")
	if err != nil {
		t.Fatalf("new GC lock: %v", err)
	}
	if got, tryErr := gcLock.TryLock(t.Context()); tryErr != nil || got {
		t.Fatalf("exclusive VM lock while readers hold: got=%v err=%v", got, tryErr)
	}
	plan1.close()
	if got, tryErr := gcLock.TryLock(t.Context()); tryErr != nil || got {
		t.Fatalf("exclusive VM lock while second reader holds: got=%v err=%v", got, tryErr)
	}
	plan2.close()
	if got, tryErr := gcLock.TryLock(t.Context()); tryErr != nil || !got {
		t.Fatalf("exclusive VM lock after readers release: got=%v err=%v", got, tryErr)
	}
	if err := gcLock.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock GC lock: %v", err)
	}
}

func TestHoldBindableRedirectsRejectsMissingLiveSource(t *testing.T) {
	rootDir := t.TempDir()
	runRoot := filepath.Join(rootDir, "run", "firecracker")
	if err := os.MkdirAll(runRoot, 0o750); err != nil {
		t.Fatalf("setup run root: %v", err)
	}
	source := filepath.Join(runRoot, "live", "cow.raw")
	destination := writeTestFile(t, rootDir, "clone-cow.raw")
	src := []*types.StorageConfig{{Path: source}}
	dst := []*types.StorageConfig{{Path: destination}}

	_, err := holdBindableRedirects(t.Context(), rootDir, runRoot, src, dst, func(string) (bool, error) {
		return true, nil
	})
	if err == nil {
		t.Fatal("missing drive of a live source VM must fail")
	}
	if _, statErr := os.Lstat(source); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("live source placeholder created: %v", statErr)
	}
	lock, lockErr := vmlock.New(rootDir, "live")
	if lockErr != nil {
		t.Fatalf("new VM lock: %v", lockErr)
	}
	if got, tryErr := lock.TryLock(t.Context()); tryErr != nil || !got {
		t.Fatalf("VM lock leaked after rejection: got=%v err=%v", got, tryErr)
	}
	if err := lock.Unlock(t.Context()); err != nil {
		t.Fatalf("unlock VM lock: %v", err)
	}
}

func TestHoldBindableRedirectsRejectsUnmanagedMissingSource(t *testing.T) {
	rootDir := t.TempDir()
	runRoot := filepath.Join(rootDir, "run", "firecracker")
	if err := os.MkdirAll(runRoot, 0o750); err != nil {
		t.Fatalf("setup run root: %v", err)
	}
	destination := writeTestFile(t, rootDir, "clone-cow.raw")
	tests := []struct {
		name   string
		source string
	}{
		{"outside run root", filepath.Join(rootDir, "foreign", "cow.raw")},
		{"reserved db directory", filepath.Join(runRoot, "db", "cow.raw")},
		{"reserved clone-lock directory", filepath.Join(runRoot, hypervisor.CloneLocksDirName, "cow.raw")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []*types.StorageConfig{{Path: tt.source}}
			dst := []*types.StorageConfig{{Path: destination}}
			if _, err := holdBindableRedirects(t.Context(), rootDir, runRoot, src, dst, func(string) (bool, error) {
				return false, nil
			}); err == nil {
				t.Fatal("unmanaged missing source must fail")
			}
			if _, err := os.Lstat(tt.source); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("source path mutated: %v", err)
			}
		})
	}
}

func TestDeadSourceLeaseFencesRealGC(t *testing.T) {
	fc := newTestFC(t)
	const sourceID = "dead-golden"
	source := filepath.Join(fc.Conf.VMRunDir(sourceID), "cow.raw")
	destination := writeTestFile(t, t.TempDir(), "clone-cow.raw")
	src := []*types.StorageConfig{{Path: source}}
	dst := []*types.StorageConfig{{Path: destination}}
	plan, err := holdBindableRedirects(t.Context(), fc.Conf.RootDirPath(), fc.Conf.RunDir(), src, dst, func(id string) (bool, error) {
		rec, peekErr := fc.PeekRecord(t.Context(), id)
		return rec != nil, peekErr
	})
	if err != nil {
		t.Fatalf("hold bindable redirects: %v", err)
	}
	t.Cleanup(plan.close)
	collect := func() {
		t.Helper()
		module := fc.BuildGCModule()
		snapshot, readErr := module.ReadDB(t.Context())
		if readErr != nil {
			t.Fatalf("GC read: %v", readErr)
		}
		ids := module.Resolve(t.Context(), snapshot, nil)
		if !slices.Contains(ids, sourceID) {
			t.Fatalf("GC candidates %v do not include %s", ids, sourceID)
		}
		if collectErr := module.Collect(t.Context(), ids, snapshot); collectErr != nil {
			t.Fatalf("GC collect: %v", collectErr)
		}
	}

	collect()
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("GC removed leased placeholder: %v", err)
	}
	plan.close()
	collect()
	if _, err := os.Stat(fc.Conf.VMRunDir(sourceID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("GC did not reclaim released dead source: %v", err)
	}
}

func TestRedirectBinds(t *testing.T) {
	dir := t.TempDir()
	regular := writeTestFile(t, dir, "cow.raw")
	dst := writeTestFile(t, dir, "clone-cow.raw")
	regular2 := writeTestFile(t, dir, "data.raw")
	sc := func(p string) []*types.StorageConfig { return []*types.StorageConfig{{Path: p}} }

	tests := []struct {
		name     string
		src, dst []*types.StorageConfig
		want     [][2]string
		wantErr  bool
	}{
		{"regular source", sc(regular), sc(dst), [][2]string{{regular, dst}}, false},
		{"missing source is planned", sc(filepath.Join(dir, "gone.raw")), sc(dst), [][2]string{{filepath.Join(dir, "gone.raw"), dst}}, false},
		{"no redirects", sc(regular), sc(regular), nil, false},
		{
			"duplicate src",
			[]*types.StorageConfig{{Path: regular}, {Path: regular}},
			[]*types.StorageConfig{{Path: dst}, {Path: regular2}},
			nil,
			true,
		},
		{
			"duplicate dst",
			[]*types.StorageConfig{{Path: regular}, {Path: regular2}},
			[]*types.StorageConfig{{Path: dst}, {Path: dst}},
			nil,
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := redirectBinds(tt.src, tt.dst)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHoldBindableRedirectsRejectsSymlinkSource(t *testing.T) {
	fc := newTestFC(t)
	srcDir := fc.Conf.VMRunDir("SRC")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	target := writeTestFile(t, srcDir, "real.raw")
	link := filepath.Join(srcDir, "cow.raw")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	src := []*types.StorageConfig{{Path: link, Role: types.StorageRoleCOW}}
	dst := []*types.StorageConfig{{Path: filepath.Join(t.TempDir(), "clone.raw"), Role: types.StorageRoleCOW}}
	_, err := holdBindableRedirects(t.Context(), fc.Conf.RootDirPath(), fc.Conf.RunDir(), src, dst, func(string) (bool, error) {
		t.Fatal("record lookup must not run for a symlink source")
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("got %v, want a not-a-regular-file rejection", err)
	}
}

func newTestFC(t *testing.T) *Firecracker {
	t.Helper()
	dir := t.TempDir()
	conf := &config.Config{
		RootDir: dir,
		RunDir:  filepath.Join(dir, "run"),
		LogDir:  filepath.Join(dir, "log"),
	}
	store, err := metajson.Open(metajson.Namespace{
		Name:     hypervisor.VMNamespaceName(typ),
		FilePath: NewConfig(conf).IndexFile(),
		LockPath: NewConfig(conf).IndexLock(),
		Codec: metajson.TableCodec{Specs: []metajson.TableSpec{
			{Key: "vms", Table: hypervisor.TableRecords},
			{Key: "names", Table: hypervisor.TableNames},
			{Key: "tombstones", Table: tombstone.TableName, Optional: true},
		}},
	})
	if err != nil {
		t.Fatalf("meta store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fc, err := New(conf, nil, store)
	if err != nil {
		t.Fatalf("new firecracker: %v", err)
	}
	return fc
}

func writeTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
		t.Fatalf("setup %s: %v", name, err)
	}
	return p
}
