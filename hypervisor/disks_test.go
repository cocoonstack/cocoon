package hypervisor

import (
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestRetargetCloneDisks(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/cow.raw", Serial: CowSerial, Role: types.StorageRoleCOW},
		{Path: "/old/data-vol.raw", Serial: "vol", Role: types.StorageRoleData},
		{Path: "/old/cidata.img", RO: true, Role: types.StorageRoleCidata},
	}
	if err := RetargetCloneDisks(configs, "/new/cow.raw", "/new"); err != nil {
		t.Fatalf("RetargetCloneDisks: %v", err)
	}
	want := []string{"/old/layer.erofs", "/new/cow.raw", "/new/data-vol.raw", "/old/cidata.img"}
	for i, sc := range configs {
		if sc.Path != want[i] {
			t.Fatalf("disk[%d] (%s): path = %s, want %s", i, sc.Role, sc.Path, want[i])
		}
	}
}

func TestRetargetCloneDisksNoCOW(t *testing.T) {
	configs := []*types.StorageConfig{
		{Path: "/old/layer.erofs", RO: true, Serial: "layer0", Role: types.StorageRoleLayer},
		{Path: "/old/data-vol.raw", Serial: "vol", Role: types.StorageRoleData},
	}
	if err := RetargetCloneDisks(configs, "/new/cow.raw", "/new"); err == nil {
		t.Fatalf("RetargetCloneDisks without a COW disk: err = %v, want error", err)
	}
}
