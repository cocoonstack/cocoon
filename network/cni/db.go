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
