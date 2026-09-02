package cloudhypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/config"
)

func TestResolveExternalVolume(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	runDir := filepath.Join(base, "run")
	logDir := filepath.Join(base, "log")
	volDir := filepath.Join(base, "vols")
	for _, d := range []string{rootDir, filepath.Join(runDir, "cloudhypervisor", "vm1"), logDir, volDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	okVol := filepath.Join(volDir, "ok.raw")
	managed := filepath.Join(runDir, "cloudhypervisor", "vm1", "data-x.raw")
	for _, f := range []string{okVol, managed} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	smuggled := filepath.Join(volDir, "smuggled.raw")
	if err := os.Symlink(managed, smuggled); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	ch := &CloudHypervisor{conf: NewConfig(&config.Config{RootDir: rootDir, RunDir: runDir, LogDir: logDir})}

	wantOK, err := filepath.EvalSymlinks(okVol)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "plain external volume", path: okVol, want: wantOK},
		{name: "direct managed path refused", path: managed, wantErr: "cocoon-managed"},
		{name: "symlink into managed dir refused", path: smuggled, wantErr: "cocoon-managed"},
		{name: "missing path refused", path: filepath.Join(volDir, "absent.raw"), wantErr: "disk path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ch.resolveExternalVolume(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
