package hypervisor

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

func TestStartSequenceRelaunchesWhenTheScannedVMMDiedBeforeTheLock(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	const id = "vm-stale-scan"
	dir := seedStoppedVMWithDirs(t, b, id)
	sockPath := SocketPath(dir)

	vmm := exec.Command("bash", "-c", fmt.Sprintf("exec -a %s tail -f /dev/null %q", b.Conf.BinaryName(), sockPath))
	if err := vmm.Start(); err != nil {
		t.Fatalf("start fake VMM: %v", err)
	}
	if err := utils.WaitFor(t.Context(), 5*time.Second, time.Millisecond, func() (bool, error) {
		return utils.VerifyProcessCmdline(vmm.Process.Pid, sockPath), nil
	}); err != nil {
		t.Fatalf("fake VMM never execed: %v", err)
	}
	scan, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !slices.Contains(scan.Find(sockPath), vmm.Process.Pid) {
		t.Fatalf("scan misses the live fake VMM %d", vmm.Process.Pid)
	}
	_ = vmm.Process.Kill()
	_ = vmm.Wait()

	launched := false
	err = b.StartSequence(t.Context(), id, &scan, StartSpec{
		Launch: func(context.Context, *VMRecord, string) (int, error) {
			launched = true
			return 4242, nil
		},
	})
	if err != nil {
		t.Fatalf("StartSequence: %v", err)
	}
	if !launched {
		t.Fatal("a VMM that died after the batch scan must not count as running")
	}
}
