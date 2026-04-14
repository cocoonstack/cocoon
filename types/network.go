package types

// NetworkConfig describes a single NIC attached to a VM.
type NetworkConfig struct {
	Tap       string `json:"tap"`
	Mac       string `json:"mac"`
	NumQueues int    `json:"num_queues"` // Virtio queue count (= CPU * 2 for multi-queue).
	QueueSize int    `json:"queue_size"`

	// Backend identifies the network provider that created this NIC ("cni" or "bridge").
	// Used to select the correct provider during network recovery on VM start.
	// Empty means "cni" for backward compatibility with existing VM records.
	Backend string `json:"backend,omitempty"`

	// BridgeDev is the Linux bridge device name (e.g. "cni0", "br0").
	// Set only when Backend is "bridge"; required for recovery and GC.
	BridgeDev string `json:"bridge_dev,omitempty"`

	// NetnsPath is the network namespace path where the tap device lives.
	// Set by the network plugin at Config time; read by the hypervisor at Start time.
	// Empty when the network backend does not use network namespaces (e.g. macOS vmnet).
	NetnsPath string `json:"netns_path,omitempty"`

	// Guest-side IP configuration returned by the network plugin.
	// nil means DHCP / no static config.
	Network *Network `json:"network,omitempty"`
}

// Network holds guest-visible IP configuration for a NIC.
// All addresses are stored as human-readable strings for JSON clarity.
// All fields are omitempty — DHCP NICs have no static IP configuration.
type Network struct {
	IP      string `json:"ip,omitempty"`      // dotted decimal, e.g. "10.0.0.2"
	Gateway string `json:"gateway,omitempty"` // dotted decimal, e.g. "10.0.0.1"
	Prefix  int    `json:"prefix,omitempty"`  // CIDR prefix length, e.g. 24
}
