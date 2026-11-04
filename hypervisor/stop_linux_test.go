package hypervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/core/log"
	coretypes "github.com/projecteru2/core/types"

	"github.com/cocoonstack/cocoon/utils"
)

func TestForceDeleteStopsAVMMStartedUnderAnotherBinaryName(t *testing.T) {
	ctx := t.Context()
	b, _ := newMeteringTestBackend(t)
	const id = "vm-renamed-binary"
	dir := seedStoppedVMWithDirs(t, b, id)
	sockPath := SocketPath(dir)

	vmm := startFakeVMM(t, "previous-"+b.Conf.BinaryName(), sockPath)
	exited := make(chan struct{})
	go func() {
		_ = vmm.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = vmm.Process.Kill()
		<-exited
	})
	if err := utils.WritePIDFile(b.PIDFilePath(dir), vmm.Process.Pid); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", sockPath, err)
	}
	defer listener.Close()

	stopped := false
	stopLocked := func(ctx context.Context, _ string) error {
		stopped = true
		_ = listener.Close()
		return utils.TerminateProcess(ctx, vmm.Process.Pid, sockPath, time.Second)
	}
	scan, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	rec := recordOf(t, b, id)
	if err := b.deleteOneLocked(ctx, id, true, stopLocked, rec, scan); err != nil {
		t.Fatalf("deleteOneLocked: %v", err)
	}
	if !stopped {
		t.Fatal("force delete did not stop the VMM started under the previous binary")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the VMM started under the previous binary survived the force delete")
	}
}

func TestDeleteSkipsAScannedVMMThatExitedBeforeTheKill(t *testing.T) {
	ctx := t.Context()
	logPath := filepath.Join(t.TempDir(), "delete.log")
	if err := log.SetupLog(ctx, &coretypes.ServerLogConfig{Level: "info", Filename: logPath}, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.SetupLog(ctx, &coretypes.ServerLogConfig{Level: "info"}, "") })

	b, _ := newMeteringTestBackend(t)
	const id = "vm-zombie-scan"
	dir := seedStoppedVMWithDirs(t, b, id)
	sockPath := SocketPath(dir)

	child := startFakeVMM(t, b.Conf.BinaryName(), sockPath)
	scan, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !slices.Contains(scan.Find(sockPath), child.Process.Pid) {
		t.Fatalf("scan misses the live fake VMM child %d", child.Process.Pid)
	}
	_ = child.Process.Kill()
	if err := utils.WaitFor(ctx, 2*time.Second, 10*time.Millisecond, func() (bool, error) {
		return !utils.VerifyProcessCmdline(child.Process.Pid, sockPath), nil
	}); err != nil {
		t.Fatalf("killed child kept its cmdline: %v", err)
	}
	if !utils.IsProcessAlive(child.Process.Pid) {
		t.Fatal("precondition: the unreaped child must linger as a zombie")
	}

	rec := recordOf(t, b, id)
	if err := b.deleteOneLocked(ctx, id, false, nil, rec, scan); err != nil {
		t.Fatalf("deleteOneLocked: %v", err)
	}
	out, err := os.ReadFile(logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(out), "killed orphan VMM") {
		t.Errorf("delete reported killing an already-exited VMM:\n%s", out)
	}
}
