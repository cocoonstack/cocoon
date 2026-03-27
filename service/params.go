package service

// VMCreateParams contains all inputs for creating a VM.
type VMCreateParams struct {
	Image   string `json:"image"`          // image reference (OCI tag or cloudimg URL)
	Name    string `json:"name,omitempty"` // VM name (optional, auto-generated if empty)
	CPU     int    `json:"cpu"`
	Memory  int64  `json:"memory,omitempty"`  // bytes (already parsed from "1G" etc.)
	Storage int64  `json:"storage,omitempty"` // bytes
	NICs    int    `json:"nics,omitempty"`
	Network string `json:"network,omitempty"` // CNI conflist name
}

// VMCloneParams contains all inputs for cloning a VM from a snapshot.
type VMCloneParams struct {
	SnapshotRef string `json:"snapshot_ref"`
	Name        string `json:"name,omitempty"`
	CPU         int    `json:"cpu,omitempty"`     // 0 = inherit from snapshot
	Memory      int64  `json:"memory,omitempty"`  // 0 = inherit
	Storage     int64  `json:"storage,omitempty"` // 0 = inherit
	NICs        int    `json:"nics,omitempty"`    // 0 = inherit
	Network     string `json:"network,omitempty"`
}

// VMRestoreParams contains inputs for restoring a VM to a snapshot.
type VMRestoreParams struct {
	VMRef       string `json:"vm_ref"`
	SnapshotRef string `json:"snapshot_ref"`
	CPU         int    `json:"cpu,omitempty"`     // 0 = keep current
	Memory      int64  `json:"memory,omitempty"`  // 0 = keep current
	Storage     int64  `json:"storage,omitempty"` // 0 = keep current
}

// VMRMParams contains inputs for deleting VM(s).
type VMRMParams struct {
	Refs  []string `json:"refs"`
	Force bool     `json:"force,omitempty"` // stop running VMs before deletion
}

// SnapshotSaveParams contains inputs for saving a snapshot.
type SnapshotSaveParams struct {
	VMRef       string `json:"vm_ref"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// DebugParams contains inputs for the debug command.
type DebugParams struct {
	VMCreateParams
	MaxCPU  int    `json:"max_cpu,omitempty"`
	Balloon int    `json:"balloon,omitempty"`
	COWPath string `json:"cow_path,omitempty"`
	CHBin   string `json:"ch_bin,omitempty"`
}
