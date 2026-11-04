package cloudhypervisor

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestSnapshotVMSendsTheType(t *testing.T) {
	tests := []struct {
		name         string
		snapshotType string
		want         string
	}{
		{name: "plain snapshot omits it", want: ""},
		{name: "diff", snapshotType: snapshotTypeDiff, want: "diff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]string
			sock := serveCHHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.UnmarshalRead(r.Body, &got)
				w.WriteHeader(http.StatusNoContent)
			}))
			if err := snapshotVM(t.Context(), utils.NewSocketHTTPClient(sock), "/d", tt.snapshotType); err != nil {
				t.Fatalf("snapshotVM: %v", err)
			}
			if got["destination_url"] != "file:///d" || got["snapshot_type"] != tt.want {
				t.Errorf("payload = %v", got)
			}
		})
	}
}

func TestEndSnapshotSeriesCallsTheEndpoint(t *testing.T) {
	var path, method string
	sock := serveCHHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := endSnapshotSeries(t.Context(), utils.NewSocketHTTPClient(sock)); err != nil {
		t.Fatalf("endSnapshotSeries: %v", err)
	}
	if method != http.MethodPut || !strings.HasSuffix(path, "/vm.end-snapshot-series") {
		t.Errorf("%s %s", method, path)
	}
}

func TestRestoreAndResumeCloneKeepPausedSkipsResume(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	sock := serveCHHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, filepath.Base(r.URL.Path))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	scopeParent := t.TempDir()
	if err := os.Mkdir(filepath.Join(scopeParent, "vm-T.scope"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ch := &CloudHypervisor{
		Backend: &hypervisor.Backend{Conf: tmpScopeConf{Config: NewConfig(&config.Config{}), parent: scopeParent}},
		conf:    NewConfig(&config.Config{}),
	}
	if err := ch.restoreAndResumeClone(t.Context(), 0, sock, t.TempDir(), &cloneResumeOpts{
		vmID:        "T",
		vmCfg:       &types.VMConfig{CPU: 2},
		directBoot:  true,
		snapshotCfg: &chVMConfig{},
		keepPaused:  true,
	}); err != nil {
		t.Fatalf("restoreAndResumeClone: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(paths, "vm.resume") {
		t.Errorf("calls = %v, want no vm.resume", paths)
	}
}

func TestCloneSeriesFilesLeavesDeltasBehind(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeMigrateFile(t, src, configJSONName, `{"disks":[{"path":"/run/cow.raw"}]}`)
	for _, name := range []string{"cow.raw", memoryRangeFile + "s", "cow.raw.ref", stateJSONName} {
		writeMigrateFile(t, src, name, "x")
	}
	if _, err := cloneSeriesFiles(t.Context(), dst, src); err != nil {
		t.Fatalf("cloneSeriesFiles: %v", err)
	}
	if utils.FileExists(filepath.Join(dst, "cow.raw.ref")) {
		t.Error("the reference reached the run dir")
	}
	for _, name := range []string{"cow.raw", memoryRangeFile + "s", stateJSONName} {
		if !utils.FileExists(filepath.Join(dst, name)) {
			t.Errorf("%s missing from the run dir", name)
		}
	}
}

func TestCloneSeriesFilesFailsWithoutTheDiskDeltaOfARound(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeMigrateFile(t, src, configJSONName, `{"disks":[{"path":"/run/cow.raw"}]}`)
	writeMigrateFile(t, src, "cow.raw", "x")
	writeMigrateFile(t, src, memoryDiffPrefix+"1", "x")
	if _, err := cloneSeriesFiles(t.Context(), dst, src); err == nil {
		t.Fatal("cloneSeriesFiles accepted a round without its disk delta")
	}
}

func writeMigrateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
