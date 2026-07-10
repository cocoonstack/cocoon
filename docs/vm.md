# VM Lifecycle

States, shutdown behavior, cloud-init first boot, data disks, performance tuning, and live status.

## States

| State      | Description                                              |
| ---------- | -------------------------------------------------------- |
| `creating` | DB placeholder written, disks being prepared             |
| `created`  | Registered, hypervisor process not yet started           |
| `running`  | Hypervisor process alive, guest is up                    |
| `stopped`  | Hypervisor process exited cleanly                        |
| `error`    | Start or stop failed                                     |

### Shutdown Behavior

- **UEFI VMs (cloudimg)**: ACPI power-button → poll for graceful exit → timeout (default 30s, configurable via `stop_timeout_seconds` in config or `--timeout` flag) → SIGTERM → 5s → SIGKILL
- **Windows VMs**: ACPI power-button works with our [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) (~8-13s shutdown once fully booted). The guest ACPI handler needs ~60s from cold boot to initialize; stopping before that triggers the 30s timeout fallback. Clone-restored VMs inherit the ready ACPI state and shut down immediately. With upstream firmware, use `ssh shutdown /s /t 0` before stopping, or `--force` to skip the ACPI timeout (see [known issues](known-issues.md))
- **Direct-boot VMs (CH, OCI)**: `vm.shutdown` API → SIGTERM → 5s → SIGKILL (no ACPI support)
- **Firecracker VMs**: `SendCtrlAltDel` → SIGTERM → 5s → SIGKILL
- **Force stop** (`--force`): skip ACPI, immediate SIGTERM → SIGKILL
- **Force delete** (`vm rm --force`): same immediate path as force stop, then delete — no graceful window
- PID ownership is verified before sending signals to prevent killing unrelated processes

### Stop Flags

| Flag        | Default                | Description                                       |
| ----------- | ---------------------- | ------------------------------------------------- |
| `--force`   | `false`                | Skip graceful ACPI shutdown, immediate kill        |
| `--timeout` | `0` (use config default) | ACPI shutdown timeout in seconds                 |

## Performance Tuning

- **Hugepages**: automatically detected from `/proc/sys/vm/nr_hugepages`; when available, VM memory is backed by 2 MiB hugepages for reduced TLB pressure
- **Disk I/O**: multi-queue virtio-blk; readonly base disks keep host page cache (`direct=off`), while writable raw/qcow2 COW disks use O_DIRECT (`direct=on`) to avoid host cache buildup and guest flush storms
- **Balloon**: 25% of memory auto-returned via virtio-balloon with deflate-on-OOM and free-page reporting (VMs with < 256 MiB memory skip balloon)
- **Watchdog**: hardware watchdog enabled by default for automatic guest reset on hang

## Cloud-init & First Boot

Cloudimg VMs receive a NoCloud cidata disk (FAT12 with `CIDATA` volume label) containing:

- **meta-data**: instance ID and hostname
- **user-data**: `#cloud-config` with configurable user/password (`--user`/`--password`, defaults to `root`/`cocoon`)
- **network-config**: Netplan v2 format with MAC-matched ethernets, static IP/gateway/DNS per NIC
- **user-data write_files**: fallback `/etc/systemd/network/15-cocoon-id*.network` files matching current MAC (`MACAddress=`), used when netplan PERM-MAC matching cannot apply

The cidata disk is **automatically excluded on subsequent boots** — after the first successful start, the VM record is marked as `first_booted` and the cidata disk is no longer attached, preventing cloud-init from re-running.

Note: `--user`/`--password` only apply to **cloudimg** VMs (cloud-init). OCI VM images bake credentials at build time — every official `os-image/ubuntu/*` image ships `openssh-server` enabled with `PermitRootLogin yes` and the default `root:cocoon` credentials. Host-to-guest control plane operations (kubectl exec, kubectl logs) prefer cocoon-agent over vsock; SSH stays available as the human-on-keyboard path.

## Data Disks

`--data-disk` attaches additional virtio-blk disks beyond the rootfs/COW. Cocoon manages each disk's lifecycle (sparse raw file under the VM's runDir, optional ext4 mkfs at create time, full participation in snapshot/clone/restore), so the user only chooses size, optional fstype/mount, and DirectIO policy.

```bash
# OCI + CH: two disks, mounted manually inside the guest
cocoon vm run --data-disk size=20G,name=db --data-disk size=50G,name=cache <oci-image>

# Cloudimg + CH: cloud-init writes /etc/fstab from the spec, disks auto-mount on boot
cocoon vm run --data-disk size=20G,name=db,mount=/mnt/db <cloudimg>

# Unformatted disk, guest is responsible for mkfs and mount
cocoon vm run --data-disk size=20G,name=raw,fstype=none <oci-image>
```

### Spec

`--data-disk` accepts comma-separated `key=value` pairs (repeatable):

| Key        | Default          | Notes |
| ---------- | ---------------- | ----- |
| `size`     | required         | Minimum 16 MiB; goes through `units.RAMInBytes` so `512M`, `2G`, etc. all work |
| `name`     | `dataN` (auto)   | 1-20 chars, `[a-z][a-z0-9_-]{0,19}`, `cocoon-` prefix reserved; auto-numbered names skip any explicit one already taken |
| `fstype`   | `ext4`           | `ext4` (cocoon mkfs's it) or `none` (guest must format); xfs is not supported in Phase 1 |
| `mount`    | `/mnt/<name>`    | Cloudimg+CH only — emitted as a cloud-init `mounts:` row using `/dev/disk/by-id/virtio-<name>`. Pass `mount=` (empty) to skip auto-mount even when fstype is ext4. `fstype=none` requires `mount=`/empty |
| `directio` | `auto`           | `on` forces `direct=on`, `off` forces page-cache, `auto` inherits VM-level `--no-direct-io`. CH only — FC has no DirectIO knob and logs a warn |

### Per-Backend Behavior

| | Cloud Hypervisor | Firecracker |
|---|---|---|
| Guest device naming | `/dev/disk/by-id/virtio-<name>` (stable) and `/dev/vdX` | `/dev/vdX` only — FC has no virtio-blk serial field |
| Cloud-init `mounts:` auto-mount | Yes (cloudimg path) | N/A (FC has no cloudimg) |
| Per-disk DirectIO override | Yes | Ignored with warn |
| Snapshot/clone/restore | Yes — sidecar carries Role/MountPoint/FSType | Yes — sidecar in `cocoon.json` carries the same |

### Snapshot/Clone/Restore

Phase 1 inherits data disks 1:1: snapshot reflinks each `data-<name>.raw` into the snapshot tar, clone re-creates them under the new VM's runDir (and regenerates cidata so cloud-init re-mounts on the new identity), and restore rolls all data disks back to the snapshot timepoint along with the rootfs and memory state. Cloud Hypervisor clones can additionally CREATE fresh data disks at clone time via `--data-disk` (hot-added after restore — the snapshot's device tree itself cannot grow); removing inherited disks at clone time is not supported, and Firecracker clones reject `--data-disk`.

Restore preflight verifies sidecar integrity, file presence (vmstate, memory, COW, every `data-*.raw`), and per-index Role/Path/RO match between sidecar and CH config.json **before** killing the running VM, so a malformed or imported snapshot fails fast and leaves the live VM untouched.

## Status Monitoring

`cocoon vm status` provides real-time VM state monitoring with two modes:

```bash
# One-shot snapshot (default)
cocoon vm status

# Refresh mode — clears and redraws like `watch`
cocoon vm status --watch

# Event stream mode — appends state changes (for scripting / vk-cocoon)
cocoon vm status --event

# Filter specific VMs, custom poll interval
cocoon vm status --event -n 2 my-vm other-vm
```

State changes are detected via **fsnotify** on the VM index file (sub-second latency), with a configurable poll interval as fallback. Event mode emits `ADDED`, `MODIFIED`, and `REMOVED` lines suitable for machine consumption.
