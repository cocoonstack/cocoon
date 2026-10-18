package cloudhypervisor

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/extend/fs"
	"github.com/cocoonstack/cocoon/extend/vfio"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/types"
	"github.com/cocoonstack/cocoon/utils"
)

const (
	chStateRunning = "Running"
	chStatePaused  = "Paused"

	snapDiskPrefix = "snapdisk-"
	formatRaw      = "raw"
	formatQcow2    = "qcow2"
)

// makeBodyFn builds a device endpoint's request body from the record reloaded under the ops lock.
type makeBodyFn func(rec *hypervisor.VMRecord) any

// preCheckFn vets the under-lock vm.info snapshot before a device mutation.
type preCheckFn func(*chVMInfoResponse) (existingID string, err error)

// findIDFn resolves the device id to detach from the under-lock vm.info snapshot.
type findIDFn func(*chVMInfoResponse) (id string, remove bool, err error)

func (ch *CloudHypervisor) DiskAttach(ctx context.Context, vmRef string, spec disk.Spec) (string, error) {
	if err := spec.Normalize(); err != nil {
		return "", err
	}
	path, err := ch.conf.ResolveExternalVolume(spec.Path)
	if err != nil {
		return "", err
	}
	id := disk.DeriveID(spec.Name)
	makeBody := func(rec *hypervisor.VMRecord) any {
		return hotDisk(rec, id, &types.StorageConfig{
			Role: types.StorageRoleData, Path: path, Serial: spec.Name, RO: spec.ReadOnly, DirectIO: spec.DirectIO,
		})
	}
	return ch.attachWith(ctx, vmRef, "vm.add-disk", makeBody, id, func(info *chVMInfoResponse) (string, error) {
		return diskAttached(info, id, spec.Name, path, spec.ReadOnly)
	})
}

func (ch *CloudHypervisor) DiskAttachSnapshot(ctx context.Context, vmRef string, src disk.SnapshotSource) (string, string, error) {
	if err := src.Normalize(); err != nil {
		return "", "", err
	}
	meta, err := hypervisor.LoadSnapshotMeta(src.Dir)
	if err != nil {
		return "", "", fmt.Errorf("load snapshot metadata: %w", err)
	}
	cowFile, err := hypervisor.SnapshotCOWFile(meta)
	if err != nil {
		return "", "", err
	}
	format := snapshotDiskFormat(cowFile)
	id := disk.DeriveID(src.Name)
	hc, rec, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	if err = checkSnapshotImage(meta.StorageConfigs, src.BlobIDs, &rec); err != nil {
		return "", "", err
	}
	path := snapshotDiskPath(rec.RunDir, src.Name, format)
	existing, err := diskAttached(info, id, src.Name, path, false)
	if err != nil || existing != "" {
		return existing, format, err
	}
	if err = copySnapshotDisk(ctx, path, filepath.Join(src.Dir, cowFile), src.BasePath); err != nil {
		os.Remove(path) //nolint:errcheck,gosec
		return "", "", err
	}
	deviceID, err := addDevice(ctx, hc, "vm.add-disk", hotDisk(&rec, id, &types.StorageConfig{
		Role: types.StorageRoleData, Path: path, Serial: src.Name,
	}), id)
	if err != nil {
		os.Remove(path) //nolint:errcheck,gosec
		return "", "", err
	}
	return deviceID, format, nil
}

func (ch *CloudHypervisor) DiskDetach(ctx context.Context, vmRef, name string) error {
	if err := disk.ValidateName(name); err != nil {
		return err
	}
	id := disk.DeriveID(name)
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, bool, error) {
		if slices.ContainsFunc(info.Config.Disks, func(d chDisk) bool { return d.ID == id }) {
			return id, true, nil
		}
		if _, pending := info.DeviceTree[id]; pending {
			return id, false, nil
		}
		log.WithFunc("cloudhypervisor.DiskDetach").Warnf(ctx, "disk %q is not attached to vm %s, nothing to detach", name, vmRef)
		return "", false, nil
	}, func(rec *hypervisor.VMRecord) error {
		return removeSnapshotDisks(rec.RunDir, name)
	})
}

func (ch *CloudHypervisor) DiskList(ctx context.Context, vmRef string) ([]disk.Attached, error) {
	return ch.listWith(ctx, vmRef, func(info *chVMInfoResponse) []disk.Attached {
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
	return ch.attachWith(ctx, vmRef, "vm.add-fs", makeBody, id, func(info *chVMInfoResponse) (string, error) {
		if !info.Config.Memory.Shared {
			return "", fmt.Errorf("fs attach requires the VM to be created with --shared-memory (current memory shared=off; cannot be flipped on a running VM)")
		}
		for _, ex := range info.Config.Fs {
			if ex.Tag == spec.Tag {
				return "", fmt.Errorf("fs tag %q already attached", spec.Tag)
			}
			if ex.ID == id {
				return "", fmt.Errorf("fs id %q already attached", id)
			}
		}
		return "", nil
	})
}

func (ch *CloudHypervisor) FsDetach(ctx context.Context, vmRef, tag string) error {
	if tag == "" {
		return fmt.Errorf("tag is required")
	}
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, bool, error) {
		for _, ex := range info.Config.Fs {
			if ex.Tag == tag {
				return ex.ID, true, nil
			}
		}
		return "", false, fmt.Errorf("fs tag %q not attached", tag)
	}, nil)
}

func (ch *CloudHypervisor) FsList(ctx context.Context, vmRef string) ([]fs.Attached, error) {
	return ch.listWith(ctx, vmRef, func(info *chVMInfoResponse) []fs.Attached {
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
	return ch.attachWith(ctx, vmRef, "vm.add-device", makeBody, spec.ID, func(info *chVMInfoResponse) (string, error) {
		// stat is gated behind the running-VM check so stopped VMs surface the state error, not a host-path one.
		st, statErr := os.Stat(path)
		if statErr != nil {
			return "", fmt.Errorf("pci path %s: %w", path, statErr)
		}
		if !st.IsDir() {
			return "", fmt.Errorf("pci path %s: not a directory", path)
		}
		for _, ex := range info.Config.Devices {
			if ex.Path == path {
				return "", fmt.Errorf("device %s already attached (id=%s)", path, ex.ID)
			}
			if spec.ID != "" && ex.ID == spec.ID {
				return "", fmt.Errorf("device id %q already in use", spec.ID)
			}
		}
		return "", nil
	})
}

func (ch *CloudHypervisor) DeviceDetach(ctx context.Context, vmRef, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	return ch.detachWith(ctx, vmRef, func(info *chVMInfoResponse) (string, bool, error) {
		if slices.ContainsFunc(info.Config.Devices, func(d chDevice) bool { return d.ID == id }) {
			return id, true, nil
		}
		return "", false, fmt.Errorf("device id %q not attached", id)
	}, nil)
}

func (ch *CloudHypervisor) DeviceList(ctx context.Context, vmRef string) ([]vfio.Attached, error) {
	return ch.listWith(ctx, vmRef, func(info *chVMInfoResponse) []vfio.Attached {
		out := make([]vfio.Attached, 0, len(info.Config.Devices))
		for _, d := range info.Config.Devices {
			out = append(out, vfio.Attached{ID: d.ID, BDF: bdfFromSysfsPath(d.Path)})
		}
		return out
	})
}

// lockedDeviceOp serializes device-set mutations per VM and returns the record plus a vm.info snapshot taken under the lock, with an ownerless pause already resumed.
func (ch *CloudHypervisor) lockedDeviceOp(ctx context.Context, vmRef string) (*http.Client, hypervisor.VMRecord, *chVMInfoResponse, func(), error) {
	hc, rec, unlock, err := ch.LockedRunningOp(ctx, vmRef)
	if err != nil {
		return nil, hypervisor.VMRecord{}, nil, nil, err
	}
	info, err := getVMInfo(ctx, hc)
	if err == nil {
		info, err = convergeOrphanedPause(ctx, hc, rec.ID, info)
	}
	if err != nil {
		unlock()
		return nil, hypervisor.VMRecord{}, nil, nil, err
	}
	return hc, rec, info, unlock, nil
}

func (ch *CloudHypervisor) attachWith(ctx context.Context, vmRef, endpoint string, makeBody makeBodyFn, fallbackID string, preCheck preCheckFn) (string, error) {
	hc, rec, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return "", err
	}
	defer unlock()
	existing, checkErr := preCheck(info)
	if checkErr != nil || existing != "" {
		return existing, checkErr
	}
	return addDevice(ctx, hc, endpoint, makeBody(&rec), fallbackID)
}

func (ch *CloudHypervisor) detachWith(ctx context.Context, vmRef string, findID findIDFn, afterEject func(*hypervisor.VMRecord) error) error {
	hc, rec, info, unlock, err := ch.lockedDeviceOp(ctx, vmRef)
	if err != nil {
		return err
	}
	defer unlock()
	deviceID, remove, err := findID(info)
	if err != nil || deviceID == "" {
		return err
	}
	if remove {
		if err := removeDeviceVM(ctx, hc, deviceID); err != nil {
			return fmt.Errorf("vm.remove-device %s: %w", deviceID, err)
		}
	}
	// CH frees the slot, id and backing file only after the guest's eject ack.
	if err := waitDeviceEjected(ctx, hc, deviceID); err != nil {
		return fmt.Errorf("device %s removal initiated but the guest has not ejected it: %w", deviceID, err)
	}
	if afterEject == nil {
		return nil
	}
	return afterEject(&rec)
}

// listWith returns nil (not error) for stopped VMs so inspect can omit the field.
func (ch *CloudHypervisor) listWith[A any](ctx context.Context, vmRef string, extract func(*chVMInfoResponse) []A) ([]A, error) {
	hc, _, err := ch.RunningVMClient(ctx, vmRef)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	info, err := getVMInfo(ctx, hc)
	if err != nil {
		return nil, err
	}
	return extract(info), nil
}

func addDevice(ctx context.Context, hc *http.Client, endpoint string, body any, fallbackID string) (string, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", endpoint, err)
	}
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
	// VFIO without --id has no detach key, so an empty fallback must fail.
	if fallbackID == "" {
		return "", fmt.Errorf("%s: empty response body and no fallback id (CH returned no PciDeviceInfo)", endpoint)
	}
	return fallbackID, nil
}

// The CH fork refuses disks without an explicit image_type; build from the record reloaded under the ops lock so DirectIO/queue semantics match create-path data disks (restore rewrites Config).
func hotDisk(rec *hypervisor.VMRecord, id string, sc *types.StorageConfig) chDisk {
	d := storageConfigToDisk(sc, rec.Config.CPU, rec.Config.DiskQueueSize, rec.Config.NoDirectIO, hypervisor.QueueCPUs(rec))
	d.ID = id
	return d
}

func diskAttached(info *chVMInfoResponse, id, name, path string, readOnly bool) (string, error) {
	for _, ex := range info.Config.Disks {
		if ex.ID == id {
			if ex.Path == path && ex.ReadOnly == readOnly {
				return id, nil
			}
			return "", fmt.Errorf("disk name %q already attached with a different path or mode", name)
		}
		// Record data disks carry CH auto ids — match serials too, else two devices race for one /dev/disk/by-id/virtio-<name>.
		if ex.Serial == name {
			return "", fmt.Errorf("disk serial %q already used by disk %q", name, ex.ID)
		}
		if ex.Path == path {
			return "", fmt.Errorf("disk path %q already attached as %q", path, ex.ID)
		}
	}
	return "", nil
}

// the ops lock plus the Running gate exclude every capture window, so a pause seen here is ownerless.
func convergeOrphanedPause(ctx context.Context, hc *http.Client, vmID string, info *chVMInfoResponse) (*chVMInfoResponse, error) {
	if info.State != chStatePaused {
		return info, nil
	}
	log.WithFunc("cloudhypervisor.convergeOrphanedPause").
		Warnf(ctx, "vm %s is paused with no capture in flight (interrupted snapshot or hibernate), resuming", vmID)
	if err := resumeVM(ctx, hc); err != nil {
		return nil, fmt.Errorf("resume orphaned pause: %w", err)
	}
	return getVMInfo(ctx, hc)
}

// CH may report a non-PCI host path, which has no BDF.
func bdfFromSysfsPath(p string) string {
	bdf, ok := strings.CutPrefix(p, vfio.SysfsPCIPrefix)
	if !ok {
		return ""
	}
	return bdf
}

// checkSnapshotImage compares digests only, so a sidecar written under another node's root still matches.
func checkSnapshotImage(sidecar []*types.StorageConfig, blobIDs map[string]struct{}, rec *hypervisor.VMRecord) error {
	snapLayers, vmLayers := layerHexes(sidecar), layerHexes(rec.StorageConfigs)
	if slices.Equal(snapLayers, vmLayers) && maps.Equal(blobIDs, rec.ImageBlobIDs) {
		return nil
	}
	return fmt.Errorf("snapshot image (layers %v, blobs %v) does not match vm %s (layers %v, blobs %v)",
		snapLayers, slices.Sorted(maps.Keys(blobIDs)), rec.ID, vmLayers, slices.Sorted(maps.Keys(rec.ImageBlobIDs)))
}

func layerHexes(configs []*types.StorageConfig) []string {
	var out []string
	for _, sc := range configs {
		if sc != nil && sc.Role == types.StorageRoleLayer {
			out = append(out, hypervisor.BlobHexFromPath(sc.Path))
		}
	}
	return out
}

func snapshotDiskFormat(cowFile string) string {
	if filepath.Ext(cowFile) == "."+formatQcow2 {
		return formatQcow2
	}
	return formatRaw
}

func snapshotDiskPath(runDir, name, format string) string {
	return filepath.Join(runDir, snapDiskPrefix+name+"."+format)
}

// The copy is throwaway, so it skips fsync; the rebase replaces a backing path that may name another node's root.
func copySnapshotDisk(ctx context.Context, dst, src, basePath string) error {
	if err := utils.ReflinkCopy(ctx, dst, src, utils.NoSync); err != nil {
		return fmt.Errorf("copy snapshot disk %s: %w", filepath.Base(src), err)
	}
	if basePath == "" {
		return nil
	}
	if err := utils.RunQemuImg(ctx, "rebase", "-u", "-F", formatQcow2, "-b", basePath, dst); err != nil {
		return fmt.Errorf("rebase snapshot disk onto %s: %w", basePath, err)
	}
	return nil
}

func removeSnapshotDisks(runDir, name string) error {
	var errs []error
	for _, format := range []string{formatRaw, formatQcow2} {
		if err := os.Remove(snapshotDiskPath(runDir, name, format)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove snapshot disk copy: %w", err))
		}
	}
	return errors.Join(errs...)
}
