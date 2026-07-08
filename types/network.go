package types

import "fmt"

// Network backend identifiers stored in NetworkConfig.Backend.
const (
	BackendCNI    = "cni"
	BackendBridge = "bridge"
)

// NetworkConfig describes a single NIC attached to a VM.
type NetworkConfig struct {
	TAP       string `json:"tap"`
	MAC       string `json:"mac"`
	NumQueues int    `json:"num_queues"` // Virtio queue count (= CPU * 2 for multi-queue).
	QueueSize int    `json:"queue_size"`

	// Backend is the provider type ("cni" or "bridge"); empty means "cni" (pre-bridge records).
	Backend string `json:"backend,omitempty"`

	// BridgeDev is the Linux bridge device name; set only when Backend=="bridge".
	BridgeDev string `json:"bridge_dev,omitempty"`

	// NetnsPath is the netns where the TAP lives; empty for backends without netns (e.g. macOS vmnet).
	NetnsPath string `json:"netns_path,omitempty"`

	// Network is the guest-visible IP config; nil means DHCP.
	Network *Network `json:"network,omitempty"`
}

// Network is the guest-visible IP config for a NIC; all fields omitempty so DHCP NICs serialize empty.
type Network struct {
	IP      string `json:"ip,omitempty"`      // dotted decimal, e.g. "10.0.0.2"
	Gateway string `json:"gateway,omitempty"` // dotted decimal, e.g. "10.0.0.1"
	Prefix  int    `json:"prefix,omitempty"`  // CIDR prefix length, e.g. 24
}

// ValidateNetworkConfigs rejects nil NIC entries (null in a persisted record) at load boundaries so downstream consumers may dereference freely.
func ValidateNetworkConfigs(configs []*NetworkConfig) error {
	for i, nc := range configs {
		if nc == nil {
			return fmt.Errorf("network config %d: nil", i)
		}
	}
	return nil
}
