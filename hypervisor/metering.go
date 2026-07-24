package hypervisor

import (
	"context"
	"time"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
)

func (b *Backend) makeEntry(kind metering.Kind, vmID string, reason metering.Reason, shape metering.Shape, now time.Time) metering.Entry {
	return metering.Entry{
		Kind: kind, VMID: vmID, Reason: reason,
		Hypervisor: b.Typ, Shape: shape, EmittedAt: now,
	}
}

func (b *Backend) makeSourceEntry(kind metering.Kind, vmID, sourceSnapshotID string, reason metering.Reason, shape metering.Shape, now time.Time) metering.Entry {
	e := b.makeEntry(kind, vmID, reason, shape, now)
	e.SourceSnapshotID = sourceSnapshotID
	return e
}

func (b *Backend) emitAll(ctx context.Context, entries []metering.Entry) {
	for _, e := range entries {
		b.Metering.Emit(ctx, e)
	}
}

// now comes from the caller so adjacent stop/start timestamps stay aligned.
func (b *Backend) emitOpenInterval(ctx context.Context, vm *types.VM, reason metering.Reason, sourceSnapshotID string, now time.Time) {
	shape := shapeFromConfig(vm.Config)
	for _, kind := range []metering.Kind{metering.KindVMStorageStart, metering.KindVMComputeStart} {
		b.Metering.Emit(ctx, b.makeSourceEntry(kind, vm.ID, sourceSnapshotID, reason, shape, now))
	}
}

func (b *Backend) emitDeleteClose(ctx context.Context, vmID string, shape metering.Shape, computeReason metering.Reason, hadRunningInterval bool) {
	now := timeNow()
	if hadRunningInterval {
		b.Metering.Emit(ctx, b.makeEntry(metering.KindVMComputeStop, vmID, computeReason, shape, now))
	}
	b.Metering.Emit(ctx, b.makeEntry(metering.KindVMStorageStop, vmID, metering.ReasonVMRemove, shape, now))
}

func shapeFromConfig(c types.VMConfig) metering.Shape {
	return metering.Shape{
		CPU:          c.CPU,
		MemBytes:     c.Memory,
		StorageBytes: c.Storage,
	}
}
