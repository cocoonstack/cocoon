package firecracker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
)

const pidFileName = "fc.pid"

var runtimeFiles = []string{hypervisor.APISocketName, pidFileName, hypervisor.ConsoleSockName}

// preflightRestore loads the FC sidecar, runs structural validation, asserts
// the role sequence matches the live record's prefix, and confirms FC's
// snapshot/load runtime files (vmstate + mem) are present. Called before
// killing the running VM so a malformed snapshot is rejected without outage.
//
// FC has no config.json equivalent to compare against (vmstate is binary),
// so the only cocoon-side disk-shape check is the sidecar itself.
func (fc *Firecracker) preflightRestore(srcDir string, rec *hypervisor.VMRecord) error {
	meta, err := loadSnapshotMeta(srcDir, fc.conf.RootDir, fc.conf.Config.RunDir)
	if err != nil {
		return err
	}
	if err := hypervisor.ValidateSnapshotIntegrity(srcDir, meta.StorageConfigs); err != nil {
		return err
	}
	for _, fname := range []string{snapshotVMStateFile, snapshotMemFile} {
		if _, statErr := os.Stat(filepath.Join(srcDir, fname)); statErr != nil {
			return fmt.Errorf("snapshot file %s missing: %w", fname, statErr)
		}
	}
	return hypervisor.ValidateRoleSequence(meta.StorageConfigs, rec.StorageConfigs)
}
