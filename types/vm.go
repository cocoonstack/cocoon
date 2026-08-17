package types

import (
	"fmt"
	"regexp"
	"time"
)

const (
	VMStateCreating VMState = "creating" // DB placeholder written, dirs/disks being prepared
	VMStateCreated  VMState = "created"  // registered, CH process not yet started
	VMStateRunning  VMState = "running"  // CH process alive, guest is up
	VMStateStopped  VMState = "stopped"  // CH process has exited cleanly
	VMStateError    VMState = "error"    // start, stop, or restore failed

	TransitionCreate         TransitionReason = "create"
	TransitionBoot           TransitionReason = "boot"
	TransitionRestart        TransitionReason = "restart"
	TransitionClone          TransitionReason = "clone"
	TransitionRestore        TransitionReason = "restore"
	TransitionStopUser       TransitionReason = "stop-user"
	TransitionError          TransitionReason = "error"
	TransitionUnexpectedExit TransitionReason = "unexpected-exit" // any exit outside a cocoon stop; an adopted process cannot report its cause
)

var (
	validName         = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	validSnapshotName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,62}$`)
	validUsername     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// shellUnsafe rejects chars that break the chpasswd YAML scalar in cidata.
	shellUnsafe = regexp.MustCompile("[`$;|&(){}\\\\<>!'\"\\x00-\\x1f\\x7f]")
)

// VMState represents the lifecycle state of a VM.
type VMState string

// TransitionReason annotates why a VM last changed state.
type TransitionReason string

// VMConfig describes the resources requested for a new VM.
type VMConfig struct {
	Config
	Name string `json:"name"`

	RestoreMode string         `json:"-"` // memory restore mode: copy|ondemand|mmap (CH only); transient, not persisted
	User        string         `json:"-"`
	Password    string         `json:"-"`
	DataDisks   []DataDiskSpec `json:"-"` // populated from --data-disk; consumed by Create
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
	if cfg.Mergeable && (cfg.HugePages || cfg.SharedMemory) {
		return fmt.Errorf("--mergeable needs plain private memory; drop --hugepages/--shared-memory")
	}
	if cfg.User != "" && !validUsername.MatchString(cfg.User) {
		return fmt.Errorf("--user %q is invalid: must be a lowercase Linux username (letters, digits, underscores, hyphens)", cfg.User)
	}
	if cfg.Password != "" && shellUnsafe.MatchString(cfg.Password) {
		return fmt.Errorf("--password contains unsafe shell or YAML characters")
	}
	return nil
}

// NetSetup is the VM's host networking state, embedded in VM and reused as the initNetwork → hypervisor handoff.
type NetSetup struct {
	NetBackend     string           `json:"net_backend,omitempty"`
	NetnsPath      string           `json:"netns_path,omitempty"`
	NetBridgeDev   string           `json:"net_bridge_dev,omitempty"`
	NetworkConfigs []*NetworkConfig `json:"network_configs,omitempty"`
}

// VM is the runtime record for a VM, persisted by the hypervisor backend.
type VM struct {
	ID         string   `json:"id"`
	Hypervisor string   `json:"hypervisor,omitempty"`
	State      VMState  `json:"state"`
	Config     VMConfig `json:"config"`

	// Runtime — populated only while State == VMStateRunning.
	PID         int    `json:"pid"`
	SocketPath  string `json:"socket_path,omitempty"`  // CH API Unix socket
	VsockSocket string `json:"vsock_socket,omitempty"` // hybrid vsock UDS for cocoon-agent

	NetSetup

	StorageConfigs []*StorageConfig `json:"storage_configs,omitempty"`

	// FirstBooted is set after the first start; skips cidata attachment on later starts (cloudimg only).
	FirstBooted bool `json:"first_booted"`

	// SnapshotIDs tracks snapshots created from this VM; populated by toVM() from VMRecord.SnapshotIDs.
	SnapshotIDs map[string]struct{} `json:"snapshot_ids,omitempty"`

	// TransitionGeneration increments once per committed transition, letting a consumer spot transitions it missed.
	TransitionGeneration uint64           `json:"transition_generation,omitempty"`
	LastTransitionReason TransitionReason `json:"last_transition_reason,omitempty"`
	LastTransitionAt     *time.Time       `json:"last_transition_at,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
}

// ResolvedNetnsPath returns NetnsPath, with NIC[0] fallback.
func (v *VM) ResolvedNetnsPath() string {
	if v == nil {
		return ""
	}
	if v.NetnsPath != "" {
		return v.NetnsPath
	}
	if nic := v.firstNIC(); nic != nil {
		return nic.NetnsPath
	}
	return ""
}

// ResolvedNetBackend returns NetBackend, with NIC[0] fallback.
func (v *VM) ResolvedNetBackend() string {
	if v == nil {
		return ""
	}
	if v.NetBackend != "" {
		return v.NetBackend
	}
	if nic := v.firstNIC(); nic != nil {
		if nic.Backend != "" {
			return nic.Backend
		}
		return BackendCNI
	}
	return ""
}

// ResolvedNetBridgeDev returns NetBridgeDev, with NIC[0] fallback.
func (v *VM) ResolvedNetBridgeDev() string {
	if v == nil {
		return ""
	}
	if v.NetBridgeDev != "" {
		return v.NetBridgeDev
	}
	if nic := v.firstNIC(); nic != nil {
		return nic.BridgeDev
	}
	return ""
}

func (v *VM) firstNIC() *NetworkConfig {
	if v == nil || len(v.NetworkConfigs) == 0 {
		return nil
	}
	return v.NetworkConfigs[0]
}
