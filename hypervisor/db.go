package hypervisor

import (
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// VMRecord is the persisted record for a single VM.
//
// StorageConfigs and NetworkConfigs live on the embedded types.VM so that
// a value-copy (info := rec.VM) automatically includes them — no manual
// field copying needed.  The JSON tags are on types.VM; do NOT duplicate
// them here or Go's encoding/json will silently shadow the promoted fields.
type VMRecord struct {
	types.VM

	BootConfig   *types.BootConfig   `json:"boot_config,omitempty"`    // nil for UEFI boot (cloudimg)
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"` // blob hex set for GC pinning

	// RunDir and LogDir store the absolute paths used when the VM was created.
	// Persisting them ensures cleanup succeeds even if --run-dir / --log-dir
	// differ from the values at creation time.
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`
}

// VMIndex is the top-level DB structure for a hypervisor backend.
type VMIndex struct {
	VMs   map[string]*VMRecord `json:"vms"`
	Names map[string]string    `json:"names"` // name → VM ID
}

func (idx *VMIndex) Init() {
	utils.InitNamedIndex(&idx.VMs, &idx.Names)
}

func (idx *VMIndex) Resolve(ref string) (string, error) {
	return utils.ResolveRef(idx.VMs, idx.Names, ref, ErrNotFound)
}

func (idx *VMIndex) ResolveMany(refs []string) ([]string, error) {
	return utils.ResolveRefs(idx.VMs, idx.Names, refs, ErrNotFound)
}
