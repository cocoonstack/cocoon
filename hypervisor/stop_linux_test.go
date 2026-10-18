package hypervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/projecteru2/core/log"
	coretypes "github.com/projecteru2/core/types"

	"github.com/cocoonstack/cocoon/utils"
)

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

	child := exec.Command("/bin/sh", "-c", "sleep 60; :", "sh", sockPath)
	child.Args[0] = b.Conf.BinaryName()
	if err := child.Start(); err != nil {
		t.Fatalf("start fake VMM child: %v", err)
	}
	t.Cleanup(func() { _ = child.Wait() })
	scan, err := utils.ScanProcsByBinary(b.Conf.BinaryName())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !slices.Contains(scan.Find(sockPath), child.Process.Pid) {
		t.Fatalf("scan misses the live fake VMM child %d", child.Process.Pid)
	}
	_ = child.Process.Kill()
	deadline := time.Now().Add(2 * time.Second)
	for utils.VerifyProcessCmdline(child.Process.Pid, b.Conf.BinaryName(), sockPath) {
		if time.Now().After(deadline) {
			t.Fatal("killed child kept its cmdline")
		}
		time.Sleep(10 * time.Millisecond)
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
