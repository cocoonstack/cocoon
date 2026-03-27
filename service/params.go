package service

// VMCreateParams contains all inputs for creating a VM.
type VMCreateParams struct {
	Image   string // image reference (OCI tag or cloudimg URL)
	Name    string // VM name (optional, auto-generated if empty)
	CPU     int
	Memory  int64  // bytes (already parsed from "1G" etc.)
	Storage int64  // bytes
	NICs    int
	Network string // CNI conflist name
}

// VMCloneParams contains all inputs for cloning a VM from a snapshot.
type VMCloneParams struct {
	SnapshotRef string
	Name        string
	CPU         int   // 0 = inherit from snapshot
	Memory      int64 // 0 = inherit
	Storage     int64 // 0 = inherit
	NICs        int   // 0 = inherit
	Network     string
}

// VMRestoreParams contains inputs for restoring a VM to a snapshot.
type VMRestoreParams struct {
	VMRef       string
	SnapshotRef string
	CPU         int   // 0 = keep current
	Memory      int64 // 0 = keep current
	Storage     int64 // 0 = keep current
}

// VMRMParams contains inputs for deleting VM(s).
type VMRMParams struct {
	Refs  []string
	Force bool // stop running VMs before deletion
}

// SnapshotSaveParams contains inputs for saving a snapshot.
type SnapshotSaveParams struct {
	VMRef       string
	Name        string
	Description string
}

// DebugParams contains inputs for the debug command.
type DebugParams struct {
	VMCreateParams
	MaxCPU  int
	Balloon int
	COWPath string
	CHBin   string
}
