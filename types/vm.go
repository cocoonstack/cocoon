package types

import (
	"fmt"
	"regexp"
	"time"
)

type VMState string

const (
	VMStateCreating VMState = "creating" // DB placeholder written, dirs/disks being prepared
	VMStateCreated  VMState = "created"  // registered, CH process not yet started
	VMStateRunning  VMState = "running"  // CH process alive, guest is up
	VMStateStopped  VMState = "stopped"  // CH process has exited cleanly
	VMStateError    VMState = "error"    // start or stop failed
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// VMConfig describes the resources requested for a new VM.
type VMConfig struct {
	Name           string `json:"name"`
	CPU            int    `json:"cpu"`
	Memory         int64  `json:"memory"`  // bytes
	Storage        int64  `json:"storage"` // COW disk size, bytes
	Image          string `json:"image"`
	Network        string `json:"network,omitempty"`          // CNI conflist name; empty = default
	Windows        bool   `json:"windows,omitempty"`          // Windows guest: UEFI boot, kvm_hyperv=on, no cidata
	SingleQueueNet bool   `json:"single_queue_net,omitempty"` // single-queue TAP with IFF_NO_PI (required by some hypervisors)
}

// Validate checks that VMConfig fields are within acceptable ranges.
func (cfg *VMConfig) Validate() error {
	if cfg.Name == "" {
		return fmt.Errorf("vm name cannot be empty")
	}
	if !validName.MatchString(cfg.Name) {
		return fmt.Errorf("vm name %q is invalid: must match %s (max 63 chars)", cfg.Name, validName.String())
	}
	if cfg.CPU <= 0 {
		return fmt.Errorf("--cpu must be at least 1, got %d", cfg.CPU)
	}
	if cfg.Memory < 512<<20 {
		return fmt.Errorf("--memory must be at least 512M, got %d", cfg.Memory)
	}
	if cfg.Storage < 10<<30 {
		return fmt.Errorf("--storage must be at least 10G, got %d", cfg.Storage)
	}
	return nil
}

// VM is the runtime record for a VM, persisted by the hypervisor backend.
type VM struct {
	ID     string   `json:"id"`
	State  VMState  `json:"state"`
	Config VMConfig `json:"config"`

	// Runtime — populated only while State == VMStateRunning.
	PID        int    `json:"pid,omitempty"`
	SocketPath string `json:"socket_path,omitempty"` // CH API Unix socket

	// Attached resources — promoted into VMRecord via embedding.
	NetworkConfigs []*NetworkConfig `json:"network_configs,omitempty"`
	StorageConfigs []*StorageConfig `json:"storage_configs,omitempty"`

	// FirstBooted is true after the VM has been started at least once.
	// Used to skip cidata attachment on subsequent starts (cloudimg only).
	FirstBooted bool `json:"first_booted,omitempty"`

	// SnapshotIDs tracks snapshots created from this VM.
	// Populated at runtime by toVM() from VMRecord.SnapshotIDs.
	SnapshotIDs map[string]struct{} `json:"snapshot_ids,omitempty"`

	// Timestamps.
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}
