package firecracker

import (
	"errors"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/types"
)

func TestCloneAfterExtractRejectsDataDisks(t *testing.T) {
	fc := &Firecracker{}
	_, err := fc.cloneAfterExtract(t.Context(), "id", &types.VMConfig{
		DataDisks: []types.DataDiskSpec{{Name: "v", Size: 1 << 24}},
	}, types.NetSetup{}, t.TempDir(), t.TempDir(), time.Now(), "")
	if !errors.Is(err, disk.ErrUnsupportedBackend) {
		t.Fatalf("err = %v, want disk.ErrUnsupportedBackend", err)
	}
}
