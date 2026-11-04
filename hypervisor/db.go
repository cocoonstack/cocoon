package hypervisor

import (
	"github.com/cocoonstack/cocoon/types"
)

// VMRecord is the persisted record for a single VM; JSON tags live on the embedded types.VM (duplicates would shadow the promoted fields).
type VMRecord struct {
	types.VM

	BootConfig   *types.BootConfig   `json:"boot_config,omitzero"`
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"` // blob hex set for GC pinning

	// RunDir/LogDir are persisted absolute paths so cleanup still finds them if --run-dir / --log-dir change later.
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`

	// Quarantine names why start must refuse; only a successful restore clears it.
	Quarantine string `json:"quarantine,omitempty"`

	// QuiescePending marks plumbing a stop or a killed launch may have left up, so a later pass brings it down.
	QuiescePending bool `json:"quiesce_pending,omitzero"`
}
