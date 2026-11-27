package cloudhypervisor

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	seriesDirName   = "snapshot-migrate-s1"
	seriesFinalFile = "series.final"
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

func TestCloneSeriesFilesLeavesDeltasBehind(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeMigrateFile(t, src, configJSONName, `{"disks":[{"path":"/run/cow.raw"}]}`)
	for _, name := range []string{"cow.raw", memoryRangeFile + "s", "cow.raw.ref", stateJSONName, seriesFinalFile} {
		writeMigrateFile(t, src, name, "x")
	}
	if _, err := cloneSeriesFiles(t.Context(), dst, src); err != nil {
		t.Fatalf("cloneSeriesFiles: %v", err)
	}
	for _, name := range []string{"cow.raw.ref", seriesFinalFile} {
		if utils.FileExists(filepath.Join(dst, name)) {
			t.Errorf("%s reached the run dir", name)
		}
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
	writeMigrateFile(t, src, seriesFinalFile, "")
	if _, err := cloneSeriesFiles(t.Context(), dst, src); err == nil || strings.Contains(err.Error(), "final round") {
		t.Fatalf("cloneSeriesFiles = %v, want the missing disk delta reported", err)
	}
}

func TestCloneSeriesFilesRefusesASeriesWithoutItsFinalRound(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeMigrateFile(t, src, configJSONName, `{"disks":[{"path":"/run/cow.raw"}]}`)
	writeMigrateFile(t, src, "cow.raw", "x")
	if _, err := cloneSeriesFiles(t.Context(), dst, src); err == nil || !strings.Contains(err.Error(), "final round") {
		t.Fatalf("cloneSeriesFiles = %v, want a series the source can still resume refused", err)
	}
	if utils.FileExists(filepath.Join(dst, "cow.raw")) {
		t.Error("a refused series reached the run dir")
	}
}

func TestAFinalRoundSourceStaysPaused(t *testing.T) {
	runDir := t.TempDir()
	writeMigrateFile(t, runDir, filepath.Join(seriesDirName, seriesFinalFile), "")
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.MarshalWrite(w, chVMInfoResponse{State: chStatePaused})
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	hc := &http.Client{Transport: handlerTransport{mux}}

	if _, err := convergeOrphanedPause(t.Context(), hc, "vm1", runDir); err == nil || !strings.Contains(err.Error(), "took its final round") {
		t.Errorf("convergeOrphanedPause = %v, want the final-round refusal", err)
	}
	pause := (&CloudHypervisor{}).snapshotSpec(t.Context()).Pause
	if err := pause(&hypervisor.VMRecord{RunDir: runDir}, hc); err == nil || !strings.Contains(err.Error(), "took its final round") {
		t.Errorf("snapshot Pause = %v, want the final-round refusal", err)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %v, want the paused guest left alone", calls)
	}
}

func writeMigrateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
