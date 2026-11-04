package firecracker

import (
	"testing"

	"github.com/cocoonstack/cocoon/hypervisor"
)

func TestFirecrackerIsNotAMigrator(t *testing.T) {
	if _, ok := any(&Firecracker{}).(hypervisor.Migrator); ok {
		t.Error("Firecracker claims live migration; vm migrate would stop returning ErrMigrateUnsupported")
	}
}
