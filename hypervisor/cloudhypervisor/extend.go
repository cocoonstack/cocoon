package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const chStatePaused = "Paused"

var (
	_ disk.Attacher     = (*CloudHypervisor)(nil)
	_ disk.Lister       = (*CloudHypervisor)(nil)
	_ fs.Attacher       = (*CloudHypervisor)(nil)
	_ fs.Lister         = (*CloudHypervisor)(nil)
	_ vfio.Attacher     = (*CloudHypervisor)(nil)
	_ vfio.Lister       = (*CloudHypervisor)(nil)
	_ netresize.Resizer = (*CloudHypervisor)(nil)
)

func (ch *CloudHypervisor) DiskAttach(ctx context.Context, vmRef string, spec disk.Spec) (string, error) {
	if err := spec.Normalize(); err != nil {
		return "", err
	}
	path, err := ch.resolveExternalVolume(spec.Path)
	if err != nil {
		return "", err
	}
	id := disk.DeriveID(spec.Name)
	// The CH fork refuses disks without an explicit image_type; build from the record reloaded under the ops lock so DirectIO/queue semantics match create-path data disks (restore rewrites Config).
	makeBody := func(rec *hypervisor.VMRecord) any {
		d := storageConfigToDisk(&types.StorageConfig{
			Role: types.StorageRoleData, Path: path, Serial: spec.Name, RO: spec.ReadOnly, DirectIO: spec.DirectIO,
		}, rec.Config.CPU, rec.Config.DiskQueueSize, rec.Config.NoDirectIO)
		d.ID = id
		return d
	}
	return ch.attachWith(ctx, vmRef, "vm.add-disk", makeBody, id, func(info *chVMInfoResponse) error {
		for _, ex := range info.Config.Disks {
			if ex.ID == id {
				return fmt.Errorf("disk name %q already attached", spec.Name)
			}
			// Record data disks carry CH auto ids — match serials too, else two devices race for one /dev/disk/by-id/virtio-<name>.
			if ex.Serial == spec.Name {
				return fmt.Errorf("disk serial %q already used by disk %q", spec.Name, ex.ID)
			}
			if ex.Path == path {
				return fmt.Errorf("disk path %q already attached as %q", path, ex.ID)
			}
		}
		return nil
	})
}

func (ch *CloudHypervisor) DiskDetach(ctx context.Context, vmRef, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	id := disk.DeriveID(name)
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, error) {
		if slices.ContainsFunc(info.Config.Disks, func(d chDisk) bool { return d.ID == id }) {
			return id, nil
		}
		return "", fmt.Errorf("disk %q not attached", name)
	})
}

func (ch *CloudHypervisor) DiskList(ctx context.Context, vmRef string) ([]disk.Attached, error) {
	return listWith(ctx, ch, vmRef, func(info *chVMInfoResponse) []disk.Attached {
		var out []disk.Attached
		for _, d := range info.Config.Disks {
			if name := disk.NameFromID(d.ID); name != "" {
				out = append(out, disk.Attached{ID: d.ID, Name: name, Path: d.Path, ReadOnly: d.ReadOnly})
			}
		}
		return out
	})
}

func (ch *CloudHypervisor) FsAttach(ctx context.Context, vmRef string, spec fs.Spec) (string, error) {
	if err := spec.Normalize(); err != nil {
		return "", err
	}
	id := fs.DeriveID(spec.Tag)
	makeBody := func(*hypervisor.VMRecord) any {
		return chFs{
			ID:        id,
			Tag:       spec.Tag,
			Socket:    spec.Socket,
			NumQueues: spec.NumQueues,
			QueueSize: spec.QueueSize,
		}
	}
	return ch.attachWith(ctx, vmRef, "vm.add-fs", makeBody, id, func(info *chVMInfoResponse) error {
		if !info.Config.Memory.Shared {
			return fmt.Errorf("fs attach requires the VM to be created with --shared-memory (current memory shared=off; cannot be flipped on a running VM)")
		}
		for _, ex := range info.Config.Fs {
			if ex.Tag == spec.Tag {
				return fmt.Errorf("fs tag %q already attached", spec.Tag)
			}
			if ex.ID == id {
				return fmt.Errorf("fs id %q already attached", id)
			}
		}
		return nil
	})
}

func (ch *CloudHypervisor) FsDetach(ctx context.Context, vmRef, tag string) error {
	if tag == "" {
		return fmt.Errorf("tag is required")
	}
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, error) {
		for _, ex := range info.Config.Fs {
			if ex.Tag == tag {
				return ex.ID, nil
			}
		}
		return "", fmt.Errorf("fs tag %q not attached", tag)
	})
}

func (ch *CloudHypervisor) FsList(ctx context.Context, vmRef string) ([]fs.Attached, error) {
	return listWith(ctx, ch, vmRef, func(info *chVMInfoResponse) []fs.Attached {
		out := make([]fs.Attached, 0, len(info.Config.Fs))
		for _, f := range info.Config.Fs {
			out = append(out, fs.Attached{ID: f.ID, Tag: f.Tag, Socket: f.Socket})
		}
		return out
	})
}

func (ch *CloudHypervisor) DeviceAttach(ctx context.Context, vmRef string, spec vfio.Spec) (string, error) {
	path, err := spec.NormalizedPath()
	if err != nil {
		return "", err
	}
	makeBody := func(*hypervisor.VMRecord) any {
		return chDevice{ID: spec.ID, Path: path}
	}
	return ch.attachWith(ctx, vmRef, "vm.add-device", makeBody, spec.ID, func(info *chVMInfoResponse) error {
		// stat is gated behind the running-VM check so stopped VMs surface the state error, not a host-path one.
		st, statErr := os.Stat(path)
		if statErr != nil {
			return fmt.Errorf("pci path %s: %w", path, statErr)
		}
		if !st.IsDir() {
			return fmt.Errorf("pci path %s: not a directory", path)
		}
		for _, ex := range info.Config.Devices {
			if ex.Path == path {
				return fmt.Errorf("device %s already attached (id=%s)", path, ex.ID)
			}
			if spec.ID != "" && ex.ID == spec.ID {
				return fmt.Errorf("device id %q already in use", spec.ID)
			}
		}
		return nil
	})
}

func (ch *CloudHypervisor) DeviceDetach(ctx context.Context, vmRef, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, error) {
		if slices.ContainsFunc(info.Config.Devices, func(d chDevice) bool { return d.ID == id }) {
			return id, nil
		}
		return "", fmt.Errorf("device id %q not attached", id)
	})
}

func (ch *CloudHypervisor) DeviceList(ctx context.Context, vmRef string) ([]vfio.Attached, error) {
	return listWith(ctx, ch, vmRef, func(info *chVMInfoResponse) []vfio.Attached {
		out := make([]vfio.Attached, 0, len(info.Config.Devices))
		for _, d := range info.Config.Devices {
			out = append(out, vfio.Attached{ID: d.ID, BDF: bdfFromSysfsPath(d.Path)})
		}
		return out
	})
}

// resolveExternalVolume canonicalizes path (EvalSymlinks also asserts existence) and refuses cocoon-managed roots — vm rm / GC delete those trees, and a symlink must not smuggle one past the check.
func (ch *CloudHypervisor) resolveExternalVolume(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("disk path: %w", err)
	}
	for _, dir := range []string{ch.conf.RootDir, ch.conf.Config.RunDir, ch.conf.Config.LogDir} {
		if r, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
			dir = r
		}
		if hypervisor.IsUnderDir(resolved, dir) {
			return "", fmt.Errorf("external volume path %s is inside a cocoon-managed directory", path)
		}
	}
	return resolved, nil
}

// inspectRunning gates on a live VM and returns a fresh vm.info for conflict/memory/device-id lookups.
func (ch *CloudHypervisor) inspectRunning(ctx context.Context, vmRef string) (*http.Client, *chVMInfoResponse, error) {
	hc, _, _, err := ch.runningVMClientWithRecord(ctx, vmRef)
	if err != nil {
		return nil, nil, err
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return nil, nil, err
	}
	return hc, info, nil
}

// lockedDeviceOp serializes device-set mutations per VM and returns the record plus a vm.info snapshot taken under the lock, making precheck-then-call atomic against concurrent attach/detach/restore.
func (ch *CloudHypervisor) lockedDeviceOp(ctx context.Context, vmRef string) (*http.Client, hypervisor.VMRecord, *chVMInfoResponse, func(), error) {
	hc, vmID, _, err := ch.runningVMClientWithRecord(ctx, vmRef)
	if err != nil {
		return nil, hypervisor.VMRecord{}, nil, nil, err
	}
	unlock, err := ch.LockVMOps(ctx, vmID)
	if err != nil {
		return nil, hypervisor.VMRecord{}, nil, nil, err
	}
	fail := func(err error) (*http.Client, hypervisor.VMRecord, *chVMInfoResponse, func(), error) {
		unlock()
		return nil, hypervisor.VMRecord{}, nil, nil, err
	}
	rec, err := ch.LoadRecord(ctx, vmID)
	if err != nil {
		return fail(err)
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return fail(err)
	}
	return hc, rec, info, unlock, nil
}

func (ch *CloudHypervisor) attachWith(
	ctx context.Context, vmRef, endpoint string,
	makeBody func(rec *hypervisor.VMRecord) any, fallbackID string,
	preCheck func(*chVMInfoResponse) error,
) (string, error) {
	hc, rec, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err = ensureNotPaused(info); err != nil {
		return "", err
	}
	if checkErr := preCheck(info); checkErr != nil {
		return "", checkErr
	}
	bodyBytes, err := json.Marshal(makeBody(&rec))
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", endpoint, err)
	}
	// vmAPIOnce: vm.add-fs / vm.add-device aren't idempotent — a retry after a lost ACK echoes as "duplicate id".
	resp, err := vmAPIOnce(ctx, hc, endpoint, bodyBytes, http.StatusOK, http.StatusNoContent)
	if err != nil {
		return "", fmt.Errorf("%s: %w", endpoint, err)
	}
	pci, err := decodePciDeviceInfo(resp)
	if err != nil {
		return "", err
	}
	if pci.ID != "" {
		return pci.ID, nil
	}
	// Body-less success means we accepted the alt 204 code; fall back to the user-supplied id, but reject empty (VFIO without --id has no detach key).
	if fallbackID == "" {
		return "", fmt.Errorf("%s: empty response body and no fallback id (CH returned no PciDeviceInfo)", endpoint)
	}
	return fallbackID, nil
}

func (ch *CloudHypervisor) detachWith(
	ctx context.Context, vmRef string,
	findID func(*chVMInfoResponse) (string, error),
) error {
	hc, _, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return err
	}
	defer unlock()
	if err = ensureNotPaused(info); err != nil {
		return err
	}
	deviceID, err := findID(info)
	if err != nil {
		return err
	}
	if err := removeDeviceVM(ctx, hc, deviceID); err != nil {
		return fmt.Errorf("vm.remove-device %s: %w", deviceID, err)
	}
	// Block until the guest acks the ACPI eject (B0EJ): only then has CH freed the slot, id, and backing file (Windows can take 10-20s).
	if err := waitDeviceEjected(ctx, hc, deviceID); err != nil {
		return fmt.Errorf("device %s removal initiated but the guest has not ejected it: %w", deviceID, err)
	}
	return nil
}

// runningVMClientWithRecord asserts the CH process is alive and returns an http.Client on its API socket plus the resolved record.
func (ch *CloudHypervisor) runningVMClientWithRecord(ctx context.Context, vmRef string) (*http.Client, string, hypervisor.VMRecord, error) {
	vmID, rec, err := ch.ResolveAndLoad(ctx, vmRef)
	if err != nil {
		return nil, "", hypervisor.VMRecord{}, err
	}
	if rec.State != types.VMStateRunning {
		return nil, "", hypervisor.VMRecord{}, fmt.Errorf("vm %s is %s: %w", vmID, rec.State, hypervisor.ErrNotRunning)
	}
	sockPath := hypervisor.SocketPath(rec.RunDir)
	pid, pidErr := utils.ReadPIDFile(ch.PIDFilePath(rec.RunDir))
	if pidErr != nil {
		return nil, "", hypervisor.VMRecord{}, fmt.Errorf("vm %s read pidfile: %w: %w", vmID, pidErr, hypervisor.ErrNotRunning)
	}
	if !utils.VerifyProcessCmdline(pid, ch.conf.BinaryName(), sockPath) {
		return nil, "", hypervisor.VMRecord{}, fmt.Errorf("vm %s pid %d not %s: %w", vmID, pid, ch.conf.BinaryName(), hypervisor.ErrNotRunning)
	}
	return utils.NewSocketHTTPClient(sockPath), vmID, rec, nil
}

// ensureNotPaused refuses device-set mutations while a capture window is open — mutating mid-capture would desync config and memory.
func ensureNotPaused(info *chVMInfoResponse) error {
	if info.State == chStatePaused {
		return fmt.Errorf("vm is paused (snapshot or hibernate in flight); retry after it completes")
	}
	return nil
}

// listWith returns nil (not error) for stopped VMs so inspect can omit the field.
func listWith[A any](
	ctx context.Context, ch *CloudHypervisor, vmRef string,
	extract func(*chVMInfoResponse) []A,
) ([]A, error) {
	_, info, err := ch.inspectRunning(ctx, vmRef)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	return extract(info), nil
}

// bdfFromSysfsPath returns the BDF suffix when path is under the canonical sysfs PCI prefix; empty otherwise (CH may report a non-PCI host path).
func bdfFromSysfsPath(p string) string {
	bdf, ok := strings.CutPrefix(p, vfio.SysfsPCIPrefix)
	if !ok {
		return ""
	}
	return bdf
}
