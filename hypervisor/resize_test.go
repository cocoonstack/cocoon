package hypervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon/metering"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

func TestResize(t *testing.T) {
	const gib = int64(1) << 30
	tests := []struct {
		name    string
		prepare func(r *VMRecord)
		spec    ResizeSpec
		want    metering.Shape
		wantErr string
	}{
		{name: "cpu and memory", spec: ResizeSpec{CPU: 4, Memory: 8 * gib}, want: metering.Shape{CPU: 4, MemBytes: 8 * gib, StorageBytes: 20 * gib}},
		{name: "one field keeps the others", spec: ResizeSpec{Memory: 8 * gib}, want: metering.Shape{CPU: 2, MemBytes: 8 * gib, StorageBytes: 20 * gib}},
		{name: "the same size changes nothing", spec: ResizeSpec{CPU: 2, Memory: 4 * gib, Storage: 20 * gib}},
		{name: "an error record without a run still open", prepare: func(r *VMRecord) { r.State = types.VMStateError }, spec: ResizeSpec{CPU: 4}, want: metering.Shape{CPU: 4, MemBytes: 4 * gib, StorageBytes: 20 * gib}},
		{name: "running", prepare: func(r *VMRecord) { r.State, r.StartedAt = types.VMStateRunning, new(timeNow()) }, spec: ResizeSpec{CPU: 4}, wantErr: "not stopped"},
		{name: "error with its compute interval open", prepare: func(r *VMRecord) { r.State, r.StartedAt = types.VMStateError, new(timeNow()) }, spec: ResizeSpec{CPU: 4}, wantErr: "not stopped"},
		{name: "creating", prepare: func(r *VMRecord) { r.State = types.VMStateCreating }, spec: ResizeSpec{CPU: 4}, wantErr: "still being created"},
		{name: "a smaller disk", spec: ResizeSpec{Storage: 10 * gib}, wantErr: "only grows"},
		{name: "memory under the floor", spec: ResizeSpec{Memory: 256 << 20}, wantErr: "--memory must be at least 512M"},
		{name: "more vCPUs than the host", spec: ResizeSpec{CPU: HostCPUCount() + 1}, wantErr: "exceeds host cores"},
		{name: "a kept burst above the new quota", prepare: func(r *VMRecord) { r.Config.CPUBurstUs = 150000 }, spec: ResizeSpec{CPU: 1}, wantErr: "--cpu-burst-us"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, ledger := newMeteringTestBackend(t)
			seedVMRecord(t, b, "vm1", 2, 4*gib, 20*gib, true)
			if tt.prepare != nil {
				if err := b.UpdateRecord(t.Context(), "vm1", func(r *VMRecord) error { tt.prepare(r); return nil }); err != nil {
					t.Fatalf("prepare: %v", err)
				}
			}
			before := shapeFromConfig(recordOf(t, b, "vm1").Config)
			ledger.Reset()

			vm, err := b.Resize(t.Context(), "vm1", tt.spec)

			after := shapeFromConfig(recordOf(t, b, "vm1").Config)
			if tt.wantErr != "" || tt.want == (metering.Shape{}) {
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Fatalf("Resize error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.wantErr == "" && err != nil {
					t.Fatalf("Resize: %v", err)
				}
				if after != before || len(ledger.Entries()) != 0 {
					t.Errorf("record %+v (was %+v), ledger %+v; want both untouched", after, before, ledger.Entries())
				}
				return
			}
			if err != nil {
				t.Fatalf("Resize: %v", err)
			}
			if after != tt.want || shapeFromConfig(vm.Config) != tt.want {
				t.Errorf("record %+v, returned %+v, want %+v", after, shapeFromConfig(vm.Config), tt.want)
			}
			entries := ledger.Entries()
			if len(entries) != 2 {
				t.Fatalf("ledger %+v, want a storage stop and a storage start", entries)
			}
			stop, start := entries[0], entries[1]
			if stop.Kind != metering.KindVMStorageStop || stop.Shape != before || stop.Reason != metering.ReasonResize {
				t.Errorf("stop %+v, want vm.storage.stop at %+v with reason resize", stop, before)
			}
			if start.Kind != metering.KindVMStorageStart || start.Shape != tt.want || start.Reason != metering.ReasonResize || !start.EmittedAt.Equal(stop.EmittedAt) {
				t.Errorf("start %+v, want vm.storage.start at %+v with reason resize at the stop's time", start, tt.want)
			}
		})
	}
}

func TestResizeGrowsTheRawCOWAndItsFilesystem(t *testing.T) {
	requireTools(t, "mkfs.ext4", "e2fsck", "resize2fs", "dumpe2fs")
	const gib = int64(1) << 30
	b, _ := newMeteringTestBackend(t)
	seedVMRecord(t, b, "vm1", 2, 4*gib, 10*gib, true)
	cow := filepath.Join(recordOf(t, b, "vm1").RunDir, types.COWRawFileName)
	if err := createSparseFile(cow, 10*gib); err != nil {
		t.Fatalf("create cow: %v", err)
	}
	if err := initCOWFilesystem(t.Context(), cow); err != nil {
		t.Fatalf("mkfs: %v", err)
	}
	if err := b.UpdateRecord(t.Context(), "vm1", func(r *VMRecord) error {
		r.StorageConfigs = []*types.StorageConfig{{Path: cow, Serial: CowSerial, Role: types.StorageRoleCOW}}
		return nil
	}); err != nil {
		t.Fatalf("record the cow: %v", err)
	}

	for _, run := range []string{"first", "repeated"} {
		if _, err := b.Resize(t.Context(), "vm1", ResizeSpec{Storage: 20 * gib}); err != nil {
			t.Fatalf("%s Resize: %v", run, err)
		}
		if st, err := os.Stat(cow); err != nil || st.Size() != 20*gib {
			t.Fatalf("%s: cow stat %v, %v; want %d bytes", run, st, err, 20*gib)
		}
		if got := ext4Bytes(t, cow); got != 20*gib {
			t.Errorf("%s: filesystem spans %d bytes, want %d", run, got, 20*gib)
		}
	}
	if got := recordOf(t, b, "vm1").Config.Storage; got != 20*gib {
		t.Errorf("recorded storage %d, want %d", got, 20*gib)
	}

	if err := os.Truncate(cow, 40*gib); err != nil {
		t.Fatalf("grow the file past the record: %v", err)
	}
	if _, err := b.Resize(t.Context(), "vm1", ResizeSpec{Storage: 30 * gib}); err == nil || !strings.Contains(err.Error(), "is already") {
		t.Errorf("Resize under the file's size = %v, want a refusal that names its size", err)
	}
	if st, err := os.Stat(cow); err != nil || st.Size() != 40*gib {
		t.Errorf("cow stat %v, %v after the refusal; want it left at %d bytes", st, err, 40*gib)
	}
}

func TestGrowCOWGrowsAQcow2ByVirtualSize(t *testing.T) {
	requireTools(t, "qemu-img")
	const gib = int64(1) << 30
	overlay := filepath.Join(t.TempDir(), "overlay.qcow2")
	if err := utils.RunQemuImg(t.Context(), "create", "-f", "qcow2", overlay, "10G"); err != nil {
		t.Fatalf("create qcow2: %v", err)
	}
	for _, tt := range []struct{ ask, want int64 }{{20 * gib, 20 * gib}, {15 * gib, 20 * gib}} {
		if err := growCOW(t.Context(), overlay, tt.ask); err != nil {
			t.Fatalf("growCOW to %d: %v", tt.ask, err)
		}
		hdr, ok, err := utils.ReadQcow2Header(overlay)
		if err != nil || !ok || hdr.VirtualSize != tt.want {
			t.Errorf("after growCOW to %d: header %+v, %v, %v; want virtual size %d", tt.ask, hdr, ok, err, tt.want)
		}
	}
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not installed", name)
		}
	}
}

func ext4Bytes(t *testing.T, path string) int64 {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "dumpe2fs", "-h", path).Output()
	if err != nil {
		t.Fatalf("dumpe2fs: %v", err)
	}
	var blocks, blockSize int64
	for line := range strings.SplitSeq(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Block count:"); ok {
			blocks = mustParseInt(t, v)
		}
		if v, ok := strings.CutPrefix(line, "Block size:"); ok {
			blockSize = mustParseInt(t, v)
		}
	}
	return blocks * blockSize
}

func mustParseInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := types.ParseSize(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}
