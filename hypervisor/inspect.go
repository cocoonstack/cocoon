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
	var result *types.VM
	return result, b.view(ctx, func(t *vmTx) error {
		id, err := t.resolve(ref)
		if err != nil {
			return err
		}
		rec, err := t.getRecord(id)
		if err != nil {
			return err
		}
		result = b.ToVM(rec)
		return nil
	})
}

// List reads every record in one transaction and runs ToVM's per-VM file IO outside it, in parallel.
func (b *Backend) List(ctx context.Context) ([]*types.VM, error) {
	var recs []*VMRecord
	if err := b.view(ctx, func(t *vmTx) error {
		all, err := t.All()
		if err != nil {
			return err
		}
		recs = slices.Collect(maps.Values(all))
		return nil
	}); err != nil {
		return nil, err
	}
	return utils.Map(ctx, recs, func(_ context.Context, _ int, r *VMRecord) (*types.VM, error) {
		return b.ToVM(r), nil
	}, b.Conf.EffectivePoolSize())
}

func (b *Backend) ToVM(rec *VMRecord) *types.VM {
	info := rec.VM
	info.Hypervisor = b.Typ
	// Clear first: clone/restore persist boot-time sockets into the record, which would leak as stale paths once the VM stops.
	info.SocketPath, info.VsockSocket, info.ConsolePath = "", "", ""
	if info.State == types.VMStateRunning {
		SetRunningSockets(&info, rec.RunDir)
		info.PID, _ = utils.ReadPIDFile(b.PIDFilePath(rec.RunDir))
	}
	info.SnapshotIDs = maps.Clone(info.SnapshotIDs)
	return &info
}

func (b *Backend) ResolveRef(ctx context.Context, ref string) (string, error) {
	var id string
	return id, b.view(ctx, func(t *vmTx) error {
		var err error
		id, err = t.resolve(ref)
		return err
	})
}

// ResolveRefs batch-resolves under a single transaction.
func (b *Backend) ResolveRefs(ctx context.Context, refs []string) ([]string, error) {
	var ids []string
	return ids, b.view(ctx, func(t *vmTx) error {
		var err error
		ids, err = t.resolveMany(refs)
		return err
	})
}

// LoadRecord returns a detached copy of the record.
func (b *Backend) LoadRecord(ctx context.Context, id string) (VMRecord, error) {
	var rec VMRecord
	return rec, b.view(ctx, func(t *vmTx) error {
		r, err := t.Get(id)
		if err != nil {
			return err
		}
		if r == nil {
			return fmt.Errorf("%q not found", id)
		}
		rec = *r
		return nil
	})
}

// ResolveAndLoad combines ResolveRef + LoadRecord in a single transaction.
func (b *Backend) ResolveAndLoad(ctx context.Context, ref string) (string, VMRecord, error) {
	var (
		id  string
		rec VMRecord
	)
	return id, rec, b.view(ctx, func(t *vmTx) error {
		var err error
		if id, err = t.resolve(ref); err != nil {
			return err
		}
		r, err := t.Get(id)
		if err != nil {
			return err
		}
		if r == nil {
			return fmt.Errorf("%q not found", id)
		}
		rec = *r
		return nil
	})
}

// UpdateRecord runs mutate on the VM's record inside one transaction and persists the result.
func (b *Backend) UpdateRecord(ctx context.Context, vmID string, mutate func(*VMRecord) error) error {
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

// SetRunningSockets fills a running VM's live sockets (API socket, bound vsock UDS, guest console) from runDir — for clone/restore records that skip ToVM.
func SetRunningSockets(info *types.VM, runDir string) {
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
