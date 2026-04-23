package types

import (
	"fmt"
	"regexp"
	"time"
)

// VMState represents the lifecycle state of a VM.
type VMState string

const (
	VMStateCreating VMState = "creating" // DB placeholder written, dirs/disks being prepared
	VMStateCreated  VMState = "created"  // registered, CH process not yet started
	VMStateRunning  VMState = "running"  // CH process alive, guest is up
	VMStateStopped  VMState = "stopped"  // CH process has exited cleanly
	VMStateError    VMState = "error"    // start or stop failed
)

var (
	validName     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	validUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// shellUnsafe matches characters that could cause shell injection in
	// cloud-init runcmd (backticks, $, semicolons, pipes, etc.).
	shellUnsafe = regexp.MustCompile("[`$;|&(){}\\\\<>!]")
)

// VMConfig describes the resources requested for a new VM.
type VMConfig struct {
	ResourceConfig
	Name string `json:"name"`

	OnDemand bool   `json:"-"` // use UFFD on-demand memory restore (CH only); transient, not persisted
	User     string `json:"-"`
	Password string `json:"-"`
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
	if cfg.QueueSize < 0 {
		return fmt.Errorf("--queue-size must be non-negative, got %d", cfg.QueueSize)
	}
	if cfg.DiskQueueSize < 0 {
		return fmt.Errorf("--disk-queue-size must be non-negative, got %d", cfg.DiskQueueSize)
	}
	if cfg.User != "" && !validUsername.MatchString(cfg.User) {
		return fmt.Errorf("--user %q is invalid: must be a lowercase Linux username (letters, digits, underscores, hyphens)", cfg.User)
	}
	if cfg.Password != "" && shellUnsafe.MatchString(cfg.Password) {
		return fmt.Errorf("--password contains unsafe shell characters (backtick, $, ;, |, &, etc.)")
	}
	return nil
}

// VM is the runtime record for a VM, persisted by the hypervisor backend.
type VM struct {
	ID         string   `json:"id"`
	Hypervisor string   `json:"hypervisor,omitempty"`
	State      VMState  `json:"state"`
	Config     VMConfig `json:"config"`

	// Runtime — populated only while State == VMStateRunning.
	PID        int    `json:"pid"`
	SocketPath string `json:"socket_path,omitempty"` // CH API Unix socket

	// Attached resources — promoted into VMRecord via embedding.
	NetworkConfigs []*NetworkConfig `json:"network_configs,omitempty"`
	StorageConfigs []*StorageConfig `json:"storage_configs,omitempty"`

	// FirstBooted is true after the VM has been started at least once.
	// Used to skip cidata attachment on subsequent starts (cloudimg only).
	FirstBooted bool `json:"first_booted"`

	// SnapshotIDs tracks snapshots created from this VM.
	// Populated at runtime by toVM() from VMRecord.SnapshotIDs.
	SnapshotIDs map[string]struct{} `json:"snapshot_ids,omitempty"`

	// Timestamps.
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}
