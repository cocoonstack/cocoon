package hypervisor

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func (b *Backend) Inspect(ctx context.Context, ref string) (*types.VM, error) {
	_, rec, err := b.ResolveAndLoad(ctx, ref)
	if err != nil {
		return nil, err
	}
	return b.ToVM(&rec), nil
}

func (b *Backend) List(ctx context.Context) ([]*types.VM, error) {
	recs, err := b.viewValue(ctx, func(t *vmTx) ([]*VMRecord, error) {
		all, err := t.All()
		return slices.Collect(maps.Values(all)), err
	})
	if err != nil {
		return nil, err
	}
	return utils.Map(ctx, recs, func(_ context.Context, _ int, r *VMRecord) (*types.VM, error) {
		return b.ToVM(r), nil
	}, b.Conf.EffectivePoolSize())
}

func (b *Backend) ToVM(rec *VMRecord) *types.VM {
	info := rec.VM
	info.Hypervisor = b.Typ
	// Clear first: clone/restore persist boot-time runtime fields into the record, which would leak as stale values once the VM stops.
	info.PID, info.SocketPath, info.VsockSocket, info.ConsolePath = 0, "", "", ""
	if info.State == types.VMStateRunning {
		setRunningSockets(&info, rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
		if !utils.VerifyProcessCmdline(info.PID, info.SocketPath) {
			info.PID = 0
		}
	}
	return &info
}

// LoadRecord returns a detached copy of the record.
func (b *Backend) LoadRecord(ctx context.Context, id string) (VMRecord, error) {
	return b.viewValue(ctx, func(t *vmTx) (VMRecord, error) { return t.loadDetached(id) })
}

// ResolveAndLoad combines resolveRef + LoadRecord in a single transaction.
func (b *Backend) ResolveAndLoad(ctx context.Context, ref string) (string, VMRecord, error) {
	var (
		id  string
		rec VMRecord
	)
	err := b.view(ctx, func(t *vmTx) error {
		var err error
		if id, err = t.resolve(ref); err != nil {
			return err
		}
		rec, err = t.loadDetached(id)
		return err
	})
	return id, rec, err
}

func (b *Backend) SetSize(ctx context.Context, ref string, cpu int, memory int64) error {
	id, err := b.resolveRef(ctx, ref)
	if err != nil {
		return err
	}
	unlock, err := b.LockVMOps(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return b.UpdateRecord(ctx, id, func(r *VMRecord) error {
		if r.State == types.VMStateRunning || r.State == types.VMStateCreating {
			return fmt.Errorf("vm %s is %s; stop it first", id, r.State)
		}
		r.Config.CPU, r.Config.Memory = cpu, memory
		return nil
	})
}

// UpdateRecord runs mutate on the VM's record inside one transaction and persists the result.
func (b *Backend) UpdateRecord(ctx context.Context, vmID string, mutate RecordHook) error {
	return b.update(ctx, func(t *vmTx) error {
		r, err := t.getRecord(vmID)
		if err != nil {
			return err
		}
		if err := mutate(r); err != nil {
			return err
		}
		return t.Put(vmID, r)
	})
}

func (b *Backend) resolveRef(ctx context.Context, ref string) (string, error) {
	return b.viewValue(ctx, func(t *vmTx) (string, error) { return t.resolve(ref) })
}

// resolveRefs batch-resolves under a single transaction.
func (b *Backend) resolveRefs(ctx context.Context, refs []string) ([]string, error) {
	return b.viewValue(ctx, func(t *vmTx) ([]string, error) { return t.ResolveMany(refs, ErrNotFound) })
}

// setRunningSockets serves clone/restore records that skip ToVM.
func setRunningSockets(info *types.VM, runDir string) {
	info.SocketPath = SocketPath(runDir)
	if p := VsockSockPath(runDir); utils.FileExists(p) {
		info.VsockSocket = p
	}
	info.ConsolePath = consolePathFromRunDir(runDir)
}

// consolePathFromRunDir prefers the console UDS (UEFI serial, FC relay); direct-boot CH VMs instead leave the PTY path saved at boot.
func consolePathFromRunDir(runDir string) string {
	if p := ConsoleSockPath(runDir); utils.FileExists(p) {
		return p
	}
	pty, err := os.ReadFile(ConsolePTYPath(runDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pty))
}
