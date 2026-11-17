package vm

import (
	"errors"
	"path/filepath"
	"testing"

	cmdcore "github.com/cocoonstack/cocoon/cmd/core"
	"github.com/cocoonstack/cocoon/config"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/hypervisor/cloudhypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestResolveRunningVMRejectsARecordWhoseVMMIsGone(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	conf := &config.Config{RootDir: dir, RunDir: filepath.Join(dir, "run"), LogDir: filepath.Join(dir, "log")}
	t.Cleanup(func() { cmdcore.CloseMetaStore(ctx) })
	hyper, err := cmdcore.InitHypervisor(ctx, conf)
	if err != nil {
		t.Fatalf("init hypervisor: %v", err)
	}
	id := utils.GenerateID()
	if err := hyper.PrereserveVM(ctx, id, &types.VMConfig{Name: "dead-vmm"}, nil); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := hyper.(*cloudhypervisor.CloudHypervisor).UpdateRecord(ctx, id, func(r *hypervisor.VMRecord) error {
		r.State = types.VMStateRunning
		return nil
	}); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	_, err = Handler{}.resolveRunningVM(ctx, conf, "exec", id)
	if !errors.Is(err, hypervisor.ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning for a running record with no VMM", err)
	}
}
