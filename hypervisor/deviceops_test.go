package hypervisor

import (
	"testing"

	"github.com/cocoonstack/cocoon/types"
)

func TestNICPersisted(t *testing.T) {
	rec := &VMRecord{}
	rec.NetworkConfigs = []*types.NetworkConfig{{MAC: "AA:BB:CC:DD:EE:01"}}
	if !NICPersisted(rec, "aa:bb:cc:dd:ee:01") {
		t.Fatal("committed NIC must be detected case-insensitively (keep device, do not tear down)")
	}
	if NICPersisted(rec, "aa:bb:cc:dd:ee:02") {
		t.Fatal("an unpersisted MAC must roll back")
	}
	if NICPersisted(nil, "aa:bb:cc:dd:ee:01") {
		t.Fatal("a missing record is not a commit")
	}
}
