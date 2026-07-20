package cni

import (
	"github.com/cocoonstack/cocoon/types"
)

// networkRecord is one NIC's persisted network state.
// Keyed by a generated network ID (unique per NIC, not per VM).
type networkRecord struct {
	types.Network `json:"network"`
	ID            string `json:"id"`
	// Type is the CNI conflist name (e.g. "cocoon", "calico").
	Type string `json:"type"`
	VMID string `json:"vm_id"`
	// IfName is the CNI interface name inside the netns (eth0, eth1, ...).
	IfName string `json:"if_name"`
}

// networkIndex is the top-level DB structure for the CNI network provider.
type networkIndex struct {
	Networks map[string]*networkRecord `json:"networks"`
}

// Init implements storage.Initer.
func (idx *networkIndex) Init() {
	if idx.Networks == nil {
		idx.Networks = make(map[string]*networkRecord)
	}
}
