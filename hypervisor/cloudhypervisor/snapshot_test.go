package cloudhypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestBuildSnapshotMetaRefusesUnrecordedDisk(t *testing.T) {
	tests := []struct {
		name    string
		diskID  string
		wantErr string
	}{
		{"hot-attached gets the detach hint", "cocoon-disk-vol1", "detach before snapshot or hibernate"},
		{"foreign unknown disk keeps the record error", "other-id", "not present in VM record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := `{"disks":[{"path":"/vols/a.raw","id":"` + tt.diskID + `"}]}`
			if err := os.WriteFile(filepath.Join(dir, configJSONName), []byte(cfg), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			rec := &hypervisor.VMRecord{}
			_, err := buildSnapshotMeta(rec, dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
