package firecracker

import (
	"github.com/cocoonstack/cocoon/hypervisor"
)

const pidFileName = "fc.pid"

var runtimeFiles = []string{hypervisor.APISocketName, pidFileName, hypervisor.ConsoleSockName}

// preflightRestore loads the FC sidecar, runs structural validation, and
// asserts the snapshot's role sequence matches the live record's prefix.
// Called before killing the running VM so a malformed snapshot can be
// rejected without an outage.
//
// FC has no config.json equivalent to compare against (vmstate is binary),
// so the only cocoon-side check is the sidecar itself.
func (fc *Firecracker) preflightRestore(srcDir string, rec *hypervisor.VMRecord) error {
	meta, err := loadSnapshotMeta(srcDir, fc.conf.RootDir, fc.conf.Config.RunDir)
	if err != nil {
		return err
	}
	if err := hypervisor.ValidateSnapshotIntegrity(srcDir, meta.StorageConfigs); err != nil {
		return err
	}
	return hypervisor.ValidateRoleSequence(meta.StorageConfigs, rec.StorageConfigs)
}
