package cni

import (
	"github.com/cocoonstack/cocoon/types"
)

// networkRecord is one NIC's persisted network state, keyed by a generated ID unique per NIC (not per VM).
type networkRecord struct {
	types.Network `json:"network"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	VMID          string `json:"vm_id"`
	IfName        string `json:"if_name"`
}
