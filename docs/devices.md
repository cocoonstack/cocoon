# Runtime Device Attach

Hot-plug vhost-user-fs shares, VFIO PCI devices, and external raw disks on a running VM (Cloud Hypervisor only).

## Overview

Cocoon can hot-plug three classes of external resources onto a running VM:

- **Vhost-user-fs** — a file share served by an external `virtiofsd` (or compatible backend) over a Unix socket. Attach surfaces a virtio-fs device in the guest, accessible via `mount -t virtiofs <tag> /mnt/...`.
- **VFIO PCI passthrough** — a host PCI device bound to `vfio-pci` (GPU, NIC, NVMe). Attach hands the device to the guest with IOMMU isolation.
- **Data disks** — an existing raw disk file, surfaced as `/dev/disk/by-id/virtio-<name>`. Cocoon never creates or deletes the backing file: it can outlive any VM and be re-attached elsewhere (a persistent volume).

All three attaches are **runtime-only**: the device lives only for the current VM process and is gone after stop/restart — re-attach after the next start. Cloud Hypervisor itself rejects snapshotting while a vhost-user or VFIO device is attached; cocoon likewise refuses `snapshot save` and `vm hibernate` while an external disk is attached — umount it in-guest, detach, capture, then re-attach after the wake/restore. External volumes are host state cocoon does not own: it never creates, copies, or deletes the backing file, and snapshot, restore, and clone carry no trace of them. All mutating verbs on one VM (attach/detach, net resize, snapshot, hibernate, restore, stop) serialize on a per-VM lock, so concurrent invocations cannot interleave with a capture window. While the VM runs, `vm inspect` reports the live attach set under `.attached_devices` (`fs`, `devices`, `disks`).

### Vhost-user-fs

Prerequisite: the VM must have been created with `--shared-memory`. CH's `memory shared=on` cannot be flipped on a running VM, and vhost-user-fs requires it to share guest memory with the backend process.

```bash
# 1) Boot VM with shared-memory enabled.
cocoon vm run --shared-memory --name share-host ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# 2) On host, run virtiofsd against a directory.
virtiofsd --socket-path=/tmp/virtiofsd.sock --shared-dir=/srv/data --cache=never &

# 3) Attach to the VM (tag is the guest mount tag and detach key).
cocoon vm fs attach share-host --socket /tmp/virtiofsd.sock --tag data

# 4) Inside the guest:
mkdir -p /mnt/data && mount -t virtiofs data /mnt/data

# 5) Detach later:
cocoon vm fs detach share-host --tag data
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--socket` | required | Absolute path to the virtiofsd unix socket |
| `--tag` | required | Guest mount tag (also detach key) |
| `--num-queues` | `1` | Request queues |
| `--queue-size` | `1024` | Queue depth |

### Data disk hot-attach

```bash
# Provision a raw disk file anywhere on the host (cocoon never touches its lifecycle).
dd if=/dev/zero of=/srv/volumes/vol1.raw bs=1M count=1024 && mkfs.ext4 /srv/volumes/vol1.raw

# Attach: name is the guest serial (/dev/disk/by-id/virtio-vol1) and the detach key.
cocoon vm disk attach my-vm --path /srv/volumes/vol1.raw --name vol1

# Inside the guest:
mount /dev/disk/by-id/virtio-vol1 /mnt/vol1

# Detach later (backing file kept; re-attach to any VM to see the same data):
cocoon vm disk detach my-vm --name vol1
```

The backing file may live anywhere outside cocoon's managed directories;
attach resolves symlinks and refuses a path under the root/run/log dirs
because `vm rm` would delete it along with them.

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--path` | required | Absolute path to an existing raw disk file |
| `--name` | required | Guest serial and detach key (`^[a-z][a-z0-9_-]{0,19}$`) |
| `--readonly` | `false` | Attach read-only |
| `--directio` | `auto` | O_DIRECT for the disk: `on`/`off`/`auto` (use `off` for files on tmpfs) |

Detach blocks until the guest acks the ACPI eject (up to 30s — Windows can
take 10–20s), so when it returns the slot, the name, and the backing file
are free for immediate reuse; a guest that never acks fails the detach with
a typed error.

The block device (`/dev/vdX`, serial visible in `lsblk -o NAME,SERIAL`) is
usable immediately after attach. The `/dev/disk/by-id/virtio-<name>` symlink
is created by guest udev and can lag or be skipped on minimal images —
`udevadm trigger --action=add --subsystem-match=block && udevadm settle`
materializes it; scripts should resolve by serial instead.

### VFIO PCI passthrough

Prerequisite: host has `intel_iommu=on` (or `amd_iommu=on`) on the kernel command line and the target PCI device is bound to `vfio-pci`.

```bash
# Bind the device on the host (one-time per device).
echo 0000:01:00.0 > /sys/bus/pci/drivers/vfio-pci/bind   # see https://wiki.archlinux.org/title/PCI_passthrough_via_OVMF

# --pci accepts: short BDF (01:00.0), full BDF (0000:01:00.0), or
# sysfs path under /sys/bus/pci/devices/. Other absolute paths are
# rejected so cocoon does not forward a non-PCI directory to CH.
cocoon vm device attach my-vm --pci 01:00.0 --id mygpu

# Detach.
cocoon vm device detach my-vm --id mygpu
```

`cocoon vm inspect VM` includes an `attached_devices` field for running VMs that surfaces every attached vhost-user-fs share, VFIO device, and hot-attached disk, read live from CH `vm.info`. The field is omitted for stopped VMs.
