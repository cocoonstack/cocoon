package hypervisor

import (
	"fmt"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

// VMRecord is the persisted record for a single VM.
// JSON tags live on the embedded types.VM — duplicating them here would shadow the promoted fields.
type VMRecord struct {
	types.VM

	BootConfig   *types.BootConfig   `json:"boot_config,omitempty"`    // nil for UEFI boot (cloudimg)
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"` // blob hex set for GC pinning

	// RunDir/LogDir are persisted absolute paths so cleanup still finds them if --run-dir / --log-dir change later.
	RunDir string `json:"run_dir,omitempty"`
	LogDir string `json:"log_dir,omitempty"`

	// Quarantine names why start must refuse (e.g. partial restore merge); only a successful restore clears it — stop's state flip cannot.
	Quarantine string `json:"quarantine,omitempty"`
}

// VMIndex is the top-level DB structure for a hypervisor backend.
type VMIndex struct {
	VMs   map[string]*VMRecord `json:"vms"`
	Names map[string]string    `json:"names"` // name → VM ID
	// OrphanDirs are migrated VM dirs whose delete removed the record but failed the dir removal; GC retries them (the orphan scan only covers the configured roots).
	OrphanDirs []string `json:"orphan_dirs,omitempty"`
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

func (idx *VMIndex) GetRecord(vmID string) (*VMRecord, error) {
	r := idx.VMs[vmID]
	if r == nil {
		return nil, fmt.Errorf("vm %s disappeared from index", vmID)
	}
	return r, nil
}
