package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestVerifyCOWSizeRejectsAShortRawDisk(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		storage int64
		wantErr bool
	}{
		{"standard tar rebuilt a short cow", 78 << 20, 10 << 30, true},
		{"cocoon export keeps the apparent size", 10 << 30, 10 << 30, false},
		{"envelope records no storage", 78 << 20, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.Create(filepath.Join(dir, types.COWRawFileName))
			if err != nil {
				t.Fatalf("create cow: %v", err)
			}
			if err = f.Truncate(tt.size); err != nil {
				t.Fatalf("truncate cow: %v", err)
			}
			if err = f.Close(); err != nil {
				t.Fatalf("close cow: %v", err)
			}

			err = VerifyCOWSize(dir, types.SnapshotConfig{Storage: tt.storage})

			if tt.wantErr && err == nil {
				t.Fatal("accepted a cow.raw the envelope says is bigger")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("rejected a valid export: %v", err)
			}
		})
	}
}

func TestVerifyCOWSizeSkipsACloudimgOverlay(t *testing.T) {
	if err := VerifyCOWSize(t.TempDir(), types.SnapshotConfig{Storage: 10 << 30}); err != nil {
		t.Fatalf("rejected a dir with no raw cow: %v", err)
	}
}

func TestReadSnapshotEnvelope_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := types.SnapshotConfig{
		ID:         "snap-xxx",
		Name:       "demo",
		Hypervisor: "cloud-hypervisor",
		NICs:       1,
	}
	if err := WriteSnapshotEnvelope(dir, cfg, []string{"cow.raw", "vmstate"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSnapshotEnvelope(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Config.ID != cfg.ID || got.Config.Name != cfg.Name || got.Config.Hypervisor != cfg.Hypervisor || got.Config.NICs != cfg.NICs {
		t.Errorf("got %+v, want %+v", got.Config, cfg)
	}
	if len(got.Files) != 2 || got.Files[0] != "cow.raw" || got.Files[1] != "vmstate" {
		t.Errorf("files = %v, want [cow.raw vmstate]", got.Files)
	}
}

func TestVerifyManifestRejectsAMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cow.raw"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(dir, []string{"cow.raw"}); err != nil {
		t.Fatalf("VerifyManifest with every file present = %v, want nil", err)
	}
	if err := VerifyManifest(dir, nil); err != nil {
		t.Fatalf("VerifyManifest without a manifest = %v, want nil for pre-manifest exports", err)
	}
	err := VerifyManifest(dir, []string{"cow.raw", "vmstate"})
	if err == nil || !strings.Contains(err.Error(), "vmstate") {
		t.Fatalf("VerifyManifest = %v, want the missing vmstate named", err)
	}
}

func TestReadSnapshotEnvelope_Missing(t *testing.T) {
	_, err := ReadSnapshotEnvelope(t.TempDir())
	if !errors.Is(err, ErrEnvelopeMissing) {
		t.Errorf("got %v, want wrap of ErrEnvelopeMissing", err)
	}
}

func TestReadSnapshotEnvelope_BadVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SnapshotJSONName),
		[]byte(`{"version": 999, "config": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadSnapshotEnvelope(dir)
	if err == nil {
		t.Fatal("want version error")
	}
}

func TestReadSnapshotEnvelope_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SnapshotJSONName),
		[]byte(`{ not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadSnapshotEnvelope(dir)
	if err == nil {
		t.Fatal("want parse error")
	}
}
