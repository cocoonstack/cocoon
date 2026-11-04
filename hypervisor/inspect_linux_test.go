package hypervisor

import (
	"os"
	"strconv"
	"testing"
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
	vmm := startFakeVMM(t, b.Conf.BinaryName(), sock)
	writeRunFile(t, b.PIDFilePath(rec.RunDir), strconv.Itoa(vmm.Process.Pid))

	if info := b.ToVM(&rec); info.PID != vmm.Process.Pid {
		t.Errorf("PID = %d, want the VMM's %d", info.PID, vmm.Process.Pid)
	}
}
