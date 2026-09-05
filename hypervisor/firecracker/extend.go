package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon/extend/disk"
	"github.com/cocoonstack/cocoon/extend/netresize"
	"github.com/cocoonstack/cocoon/hypervisor"
	"github.com/cocoonstack/cocoon/network"
	"github.com/cocoonstack/cocoon/types"
)

// fcNICOps drives Firecracker's virtio-pci NICs for the shared resize driver; removal returns as soon as the VMM drops the device, the guest releases its stale PCI node itself.
type fcNICOps struct {
	hc *http.Client
}

func (o fcNICOps) LiveNICs(ctx context.Context) ([]hypervisor.LiveNIC, error) {
	cfg, err := getVMConfig(ctx, o.hc)
	if err != nil {
		return nil, err
	}
	live := make([]hypervisor.LiveNIC, 0, len(cfg.NetworkInterfaces))
	for _, n := range cfg.NetworkInterfaces {
		idx := -1
		if _, err := fmt.Sscanf(n.IfaceID, ifaceIDFmt, &idx); err != nil {
			idx = -1
		}
		live = append(live, hypervisor.LiveNIC{ID: n.IfaceID, Index: idx})
	}
	return live, nil
}

func (o fcNICOps) AddNIC(ctx context.Context, index int, nc *types.NetworkConfig) (string, error) {
	id := fmt.Sprintf(ifaceIDFmt, index)
	return id, hotplugDevice(ctx, o.hc, "/network-interfaces/"+id, fcNetworkInterface{IfaceID: id, HostDevName: nc.TAP, GuestMAC: nc.MAC, MTU: nc.MTU}, "network-interface")
}

func (o fcNICOps) RemoveNIC(ctx context.Context, id string) error {
	return deleteDevice(ctx, o.hc, "/network-interfaces/"+id)
}

// TAPQueues is one queue pair: Firecracker opens every TAP single-queue.
func (fcNICOps) TAPQueues(int) int { return network.NetNumQueues(1) }

func (fc *Firecracker) NetResize(ctx context.Context, vmRef string, spec netresize.Spec, plumbing netresize.Plumbing) (netresize.Result, error) {
	if err := spec.Normalize(); err != nil {
		return netresize.Result{}, err
	}
	hc, vmID, rec, unlock, err := fc.lockedDeviceOp(ctx, vmRef, netresize.ErrUnsupportedBackend)
	if err != nil {
		return netresize.Result{}, err
	}
	defer unlock()
	return fc.NetResizeWith(ctx, vmID, &rec, fcNICOps{hc: hc}, plumbing, spec.Target)
}

func (fc *Firecracker) DiskAttach(ctx context.Context, vmRef string, spec disk.Spec) (string, error) {
	if err := spec.Normalize(); err != nil {
		return "", err
	}
	path, err := fc.conf.ResolveExternalVolume(spec.Path)
	if err != nil {
		return "", err
	}
	id, err := hotDiskID(spec.Name)
	if err != nil {
		return "", err
	}
	hc, vmID, _, unlock, err := fc.lockedDeviceOp(ctx, vmRef, disk.ErrUnsupportedBackend)
	if err != nil {
		return "", err
	}
	defer unlock()
	cfg, err := getVMConfig(ctx, hc)
	if err != nil {
		return "", err
	}
	if err := checkDriveFree(cfg, id, path); err != nil {
		return "", err
	}
	if spec.DirectIO != nil {
		log.WithFunc("firecracker.DiskAttach").Warnf(ctx, "directio on disk %s ignored: FC has no DirectIO knob (IoEngine=Async fixed)", spec.Name)
	}
	d := fcDrive{DriveID: id, PathOnHost: path, IsReadOnly: spec.ReadOnly}
	if !spec.ReadOnly {
		d.IoEngine = ioEngineAsync
	}
	if err := hotplugDevice(ctx, hc, "/drives/"+id, d, "drive"); err != nil {
		return "", fmt.Errorf("vm %s hot-plug drive %s: %w", vmID, id, err)
	}
	return id, nil
}

func (fc *Firecracker) DiskDetach(ctx context.Context, vmRef, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	hc, _, _, unlock, err := fc.lockedDeviceOp(ctx, vmRef, disk.ErrUnsupportedBackend)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := getVMConfig(ctx, hc)
	if err != nil {
		return err
	}
	id, err := hotDiskID(name)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(cfg.Drives, func(d fcDrive) bool { return d.DriveID == id }) {
		return fmt.Errorf("disk %q not attached", name)
	}
	return deleteDevice(ctx, hc, "/drives/"+id)
}

// DiskList returns nil (not error) for stopped and MMIO VMs so inspect can omit the field.
func (fc *Firecracker) DiskList(ctx context.Context, vmRef string) ([]disk.Attached, error) {
	if _, rec, err := fc.ResolveAndLoad(ctx, vmRef); err != nil || !rec.Config.PCI {
		return nil, err
	}
	hc, _, err := fc.RunningVMClient(ctx, vmRef)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	cfg, err := getVMConfig(ctx, hc)
	if err != nil {
		return nil, err
	}
	return hotAttachedDisks(cfg), nil
}

// lockedDeviceOp serializes device-set mutations per VM and returns the record reloaded under the lock; MMIO VMs are refused with errUnsupported because only the virtio-pci transport hot-plugs.
func (fc *Firecracker) lockedDeviceOp(ctx context.Context, vmRef string, errUnsupported error) (*http.Client, string, hypervisor.VMRecord, func(), error) {
	hc, vmID, err := fc.RunningVMClient(ctx, vmRef)
	if err != nil {
		return nil, "", hypervisor.VMRecord{}, nil, err
	}
	unlock, err := fc.LockVMOps(ctx, vmID)
	if err != nil {
		return nil, "", hypervisor.VMRecord{}, nil, err
	}
	fail := func(err error) (*http.Client, string, hypervisor.VMRecord, func(), error) {
		unlock()
		return nil, "", hypervisor.VMRecord{}, nil, err
	}
	rec, err := fc.EntryGuardLoad(ctx, vmID)
	if err != nil {
		return fail(err)
	}
	if err := requirePCI(&rec, errUnsupported); err != nil {
		return fail(err)
	}
	if err := convergeOrphanedPause(ctx, hc, vmID); err != nil {
		return fail(err)
	}
	return hc, vmID, rec, unlock, nil
}

// convergeOrphanedPause resumes a VM left paused by a dead capture; the pause is provably ownerless because callers hold the ops lock every capture window holds and RunningVMClient's Running gate excludes restore/clone windows.
func convergeOrphanedPause(ctx context.Context, hc *http.Client, vmID string) error {
	info, err := getInstanceInfo(ctx, hc)
	if err != nil {
		return err
	}
	if info.State != vmStatePaused {
		return nil
	}
	log.WithFunc("firecracker.convergeOrphanedPause").
		Warnf(ctx, "vm %s is paused with no capture in flight (interrupted snapshot or hibernate), resuming", vmID)
	if err := resumeVM(ctx, hc); err != nil {
		return fmt.Errorf("resume orphaned pause: %w", err)
	}
	return nil
}

func requirePCI(rec *hypervisor.VMRecord, errUnsupported error) error {
	if rec.Config.PCI {
		return nil
	}
	return fmt.Errorf("vm %s boots on the MMIO transport (created without --pci): %w", rec.ID, errUnsupported)
}

func hotDiskID(name string) (string, error) {
	if strings.Contains(name, "-") {
		return "", fmt.Errorf("disk name %q: Firecracker drive ids allow only letters, digits and underscores", name)
	}
	return hotDiskIDPrefix + name, nil
}

// hotDiskName reverses hotDiskID; empty for create-path drives (drive_N) and foreign ids.
func hotDiskName(id string) string {
	name, ok := strings.CutPrefix(id, hotDiskIDPrefix)
	if ok && types.ValidDataDiskName(name) {
		return name
	}
	return ""
}

func checkDriveFree(cfg *fcVMConfig, id, path string) error {
	for _, d := range cfg.Drives {
		if d.DriveID == id {
			return fmt.Errorf("disk %q already attached", hotDiskName(id))
		}
		if d.PathOnHost == path {
			return fmt.Errorf("disk path %q already attached as %q", path, d.DriveID)
		}
	}
	return nil
}

func hotAttachedDisks(cfg *fcVMConfig) []disk.Attached {
	var out []disk.Attached
	for _, d := range cfg.Drives {
		if name := hotDiskName(d.DriveID); name != "" {
			out = append(out, disk.Attached{ID: d.DriveID, Name: name, Path: d.PathOnHost, ReadOnly: d.IsReadOnly})
		}
	}
	return out
}
