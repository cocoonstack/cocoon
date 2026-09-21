package firecracker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
)

const (
	pidFileName = "fc.pid"

	directIOIgnoredMsg = "directio on disk %s ignored: FC has no DirectIO knob (IoEngine=Async fixed)"
)

var runtimeFiles = []string{hypervisor.APISocketName, pidFileName, hypervisor.ConsoleSockName, hypervisor.VsockSockName}

func (fc *Firecracker) preflightRestore(srcDir string, rec *hypervisor.VMRecord) error {
	meta, err := fc.conf.PreflightRestore(srcDir, rec, snapshotIntegrity)
	if err != nil {
		return err
	}
	return hypervisor.ValidateResidentPaths(meta.StorageConfigs, rec.StorageConfigs)
}

// snapshotIntegrity adds the vmstate and memory file checks: the sidecar is FC's only disk-shape source.
func snapshotIntegrity(srcDir string, sidecar []*types.StorageConfig) error {
	if err := hypervisor.ValidateSnapshotIntegrity(srcDir, sidecar); err != nil {
		return err
	}
	for _, fname := range []string{snapshotVMStateFile, snapshotMemFile} {
		if _, statErr := os.Stat(filepath.Join(srcDir, fname)); statErr != nil {
			return fmt.Errorf("snapshot file %s missing: %w", fname, statErr)
		}
	}
	return nil
}
