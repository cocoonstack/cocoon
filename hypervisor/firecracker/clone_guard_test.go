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

func TestValidateCloneNetworkMTUs(t *testing.T) {
	tests := []struct {
		name         string
		snapshotMTUs []int
		networks     []*types.NetworkConfig
		wantErr      bool
	}{
		{"matching", []int{9000, 1500}, []*types.NetworkConfig{{MTU: 9000}, {MTU: 1500}}, false},
		{"target MTU smaller", []int{9000}, []*types.NetworkConfig{{MTU: 1500}}, true},
		{"target MTU larger", []int{1500}, []*types.NetworkConfig{{MTU: 9000}}, true},
		{"different count", []int{9000}, nil, true},
		{"no NICs", nil, nil, false},
		{"legacy snapshot without MTUs", nil, []*types.NetworkConfig{{MTU: 9000}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCloneNetworkMTUs(tt.snapshotMTUs, tt.networks)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
