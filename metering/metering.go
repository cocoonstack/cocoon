// Package metering emits append-only VM/snapshot lifecycle endpoints; tenant attribution lives upstream. Recorder is the contract; backends live in subpackages (file, stderr, capture).
package metering

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

const (
	KindVMComputeStart   Kind = "vm.compute.start"
	KindVMComputeStop    Kind = "vm.compute.stop"
	KindVMStorageStart   Kind = "vm.storage.start"
	KindVMStorageStop    Kind = "vm.storage.stop"
	KindSnapStorageStart Kind = "snap.storage.start"
	KindSnapStorageStop  Kind = "snap.storage.stop"

	ReasonBoot       Reason = "boot"
	ReasonRestart    Reason = "restart"
	ReasonClone      Reason = "clone"
	ReasonRestore    Reason = "restore"
	ReasonStopUser   Reason = "stop-user"
	ReasonStopCrash  Reason = "stop-crash"
	ReasonVMRemove   Reason = "vm-rm"
	ReasonSnapRemove Reason = "snap-rm"
)

// Kind identifies a lifecycle endpoint; downstream pairs *.start with *.stop by id.
type Kind string

// Reason annotates why an endpoint was emitted.
type Reason string

// Shape is the resource snapshot at the moment an Entry is emitted.
type Shape struct {
	CPU          int   `json:"cpu,omitempty"`
	MemBytes     int64 `json:"mem_bytes,omitempty"`
	StorageBytes int64 `json:"storage_bytes,omitempty"`
}

var _ io.WriterTo = Entry{}

// Entry is one append-only lifecycle event.
type Entry struct {
	Kind             Kind      `json:"kind"`
	VMID             string    `json:"vm_id,omitempty"`
	SnapshotID       string    `json:"snapshot_id,omitempty"`
	SourceSnapshotID string    `json:"source_snapshot_id,omitempty"`
	Reason           Reason    `json:"reason,omitempty"`
	Hypervisor       string    `json:"hypervisor,omitempty"`
	Shape            Shape     `json:"shape"`
	EmittedAt        time.Time `json:"emitted_at"`
}

// WriteTo writes one JSONL record.
func (e Entry) WriteTo(w io.Writer) (int64, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	return int64(n), err
}

// Recorder accepts lifecycle entries; implementations must be safe for concurrent use.
type Recorder interface {
	Emit(context.Context, Entry)
}

var _ Recorder = NopRecorder{}

// NopRecorder discards every entry; zero value is usable.
type NopRecorder struct{}

func (NopRecorder) Emit(context.Context, Entry) {}
