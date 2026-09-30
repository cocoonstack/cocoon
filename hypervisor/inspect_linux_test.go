package hypervisor

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon/utils"
)

func TestToVMDropsARecycledPID(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)
	rec, err := b.LoadRecord(t.Context(), "vm1")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	writeRunFile(t, b.PIDFilePath(rec.RunDir), strconv.Itoa(os.Getpid()))

	if info := b.ToVM(&rec); info.PID != 0 {
		t.Errorf("PID = %d, want 0 for a live process that is not this VM's VMM", info.PID)
	}
}

func TestToVMKeepsTheVMMPID(t *testing.T) {
	b, _ := newMeteringTestBackend(t)
	seedRunningVM(t, b, "vm1", 1, 1<<30, 10<<30)
	rec, err := b.LoadRecord(t.Context(), "vm1")
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	sock := SocketPath(rec.RunDir)
	vmm := exec.Command("/bin/sh", "-c", "sleep 60; :", "sh", sock)
	vmm.Args[0] = b.Conf.BinaryName()
	if err := vmm.Start(); err != nil {
		t.Fatalf("start fake VMM: %v", err)
	}
	t.Cleanup(func() {
		_ = vmm.Process.Kill()
		_ = vmm.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for !utils.VerifyProcessCmdline(vmm.Process.Pid, b.Conf.BinaryName(), sock) {
		if time.Now().After(deadline) {
			t.Fatal("fake VMM did not exec in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeRunFile(t, b.PIDFilePath(rec.RunDir), strconv.Itoa(vmm.Process.Pid))

	if info := b.ToVM(&rec); info.PID != vmm.Process.Pid {
		t.Errorf("PID = %d, want the VMM's %d", info.PID, vmm.Process.Pid)
	}
}
