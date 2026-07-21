package hypervisor

import (
	"github.com/cocoonstack/cocoon/types"
)

// VMRecord is the persisted record for a single VM; JSON tags live on the embedded types.VM (duplicates would shadow the promoted fields).
type VMRecord struct {
	types.VM

	BootConfig   *types.BootConfig   `json:"boot_config,omitempty"`    // nil for UEFI boot (cloudimg)
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"` // blob hex set for GC pinning

	// RunDir/LogDir are persisted absolute paths so cleanup still finds them if --run-dir / --log-dir change later.
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`

	// Quarantine names why start must refuse (e.g. partial restore merge); only a successful restore clears it — stop's state flip cannot.
	Quarantine string `json:"quarantine,omitempty"`

	// QuiescePending survives a crash between the stop transition and the network Quiesce, so a later pass retries it.
	QuiescePending bool `json:"quiesce_pending,omitempty"`
}
