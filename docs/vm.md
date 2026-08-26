# VM Lifecycle

States, shutdown behavior, cloud-init first boot, data disks, performance tuning, and live status.

## States

| State      | Description                                              |
| ---------- | -------------------------------------------------------- |
| `creating` | DB placeholder written, disks being prepared             |
| `created`  | Registered, hypervisor process not yet started           |
| `running`  | Hypervisor process alive, guest is up                    |
| `stopped`  | Hypervisor process exited cleanly                        |
| `error`    | Start, stop, or restore failed — recover with `vm restore` |

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

## CPU Isolation (cgroup v2)

Every VM's hypervisor process is spawned directly into its own cgroup v2 scope (`<cgroup_parent>/vm-<id>.scope`, default parent `cocoon.slice`) via `CLONE_INTO_CGROUP` — vCPU threads, virtio queue workers, and io_uring kernel workers all land inside. The vCPU count alone does not bound host consumption (a 1-vCPU VM under I/O measures 111–113% of a core); the scope does.

Defaults are Kubernetes-style Guaranteed at N for `--cpu N`: quota = N cores (`--cpu` is a hard cap, not just a topology hint), weight = N (proportional share under contention), no burst. Override any raw knob: lower `--cpu-weight` for burstable overcommit, raise `--cpu-quota-us` to give the VMM's I/O service headroom beyond the guest's budget, add `--cpu-burst-us` for bounded spikes. Two caveats at defaults: a saturated VM doing I/O pays its virtio service out of the N-core budget (~13% floor case), and `cpu.weight` only arbitrates real runqueue contention — a parent bandwidth limit is consumed first-come-first-served, not by weight.

`cgroup_cpus` fences the whole VM population onto a host cpu subset (e.g. `0-14` on a 16-core host keeps core 15 for the OS, the API consumer, and clone/wake execution). `--cpuset-cpus` pins one VM to specific cores inside the fence. Both are validated by cocoon against the effective sets — the kernel silently degrades ungrantable cpuset requests rather than failing — and shrinking the fence is refused while a running VM's placement conflicts. Clearing `cgroup_cpus` converges: the stale fence is reset on the next launch.

Provisioning is exempt from the ceiling: clone/restore launch with the quota at `max` — weight, fence, and placement still apply — and the finite quota is armed after the memory load completes, before the guest resumes, so snapshot loading runs at control-plane speed while the guest never executes uncapped. The mmap mode's deferred first-touch is guest-lifetime work whose cost keys on host cache state: cold cache faults from disk — I/O-bound, invisible to the quota; hot cache (sibling clones of one golden) turns guest writes into pure-CPU copy-on-write, fully exposed at wall = cpu_time / min(quota, busy threads). Time-to-first-exec is flat either way; only bulk re-touch of a large working set feels the ceiling, and only when oversold (quota under one core per busy thread). For such shapes express low priority with `--cpu-weight` instead of a tight quota, or use `copy` mode to prepay the load inside the exempt provisioning window.

cgroup knobs are host-side policy, like networking: snapshots record the source VM's values but never apply them — a clone takes its policy from flags (defaults otherwise), restore keeps the target VM's. Scopes are removed when the VMM dies (stop, hibernate, delete, crash convergence) and orphans are swept by `cocoon gc`. `cocoon vm list` shows per-VM throttling as `THROTTLED` (`nr_throttled/throttled_usec` from `cpu.stat`).

Requirements: cgroup v2 unified hierarchy with the `cpu` controller (kernel ≥ 5.14 for burst), running cocoon as root (production shape). Non-root works inside a systemd user slice with delegated controllers (`systemd-run --user --scope`), where user slices typically delegate `cpu` but not `cpuset` — fence/placement then fail preflight with the exact missing file named.

### Recipes

```bash
# Guaranteed at N (default): hard cap 2 cores, share 2, no burst
cocoon vm run --cpu 2 --memory 2G --name vm1 ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Burstable overcommit: reach 2 cores when idle, shrink by weight under pressure
cocoon vm run --cpu 2 --cpu-weight 25 --name burst1 ...

# Headroom for virtio I/O service: guest keeps its full 2 cores under load
cocoon vm run --cpu 2 --cpu-quota-us 230000 --name io-heavy ...

# Metered: 0.5-core long-run average, bounded 1-core spikes
cocoon vm run --cpu 1 --cpu-quota-us 50000 --cpu-burst-us 50000 --name metered ...

# Pinning (NUMA / isolation-sensitive only — wastes idle cores)
cocoon vm run --cpu 2 --cpuset-cpus 2-3 --name pinned ...

# Machine fence (config, not a flag): the fleet never touches core 15
COCOON_CGROUP_CPUS=0-14 cocoon vm run ...

# Clones never inherit snapshot policy — give it explicitly or get defaults
cocoon vm clone golden --name c1                  # Guaranteed at N
cocoon vm clone golden --name c2 --cpu-weight 10  # explicit share
```

Rules of thumb: density → weight overcommit; single-VM performance → raised quota; billing semantics → quota + burst; pinning only when NUMA or isolation demands it. `cocoon vm list`'s `THROTTLED` column (count/total time from `cpu.stat`) shows who is hitting their cap.

### Reserving CPU for the control plane

The fence bounds the VMs; the caller's own work (clone, restore, the API consumer) is deliberately not cocoon's to manage — set it on the invoking service's systemd unit, which writes the same cgroup v2 files:

```ini
# /etc/systemd/system/<your-service>.service.d/cpu.conf
[Service]
CPUWeight=1000     # management plane wins contention on the VM cores
```

With `cgroup_cpus=0-14`, the reserved core 15 has no VM competition and acts as the control plane's fast lane without pinning; prefer that over `AllowedCPUs=15`, which would confine clone's multi-core memory restore to one core. One-off commands: `systemd-run --scope -p CPUWeight=1000 cocoon vm clone ...`.

## Performance Tuning

- **Hugepages** (Cloud Hypervisor only): opt-in via `vm create --hugepages`; VM memory is backed by 2 MiB hugepages for reduced TLB pressure, and in exchange snapshots of that VM restore via eager copy only (the mmap fast path needs plain private-anon memory). Firecracker rejects `--hugepages`: FC cannot restore a hugetlbfs-backed snapshot, which would break hibernate/clone
- **Mergeable memory / KSM** (Cloud Hypervisor only): opt-in via `--mergeable` at golden creation; guest memory is madvised `MADV_MERGEABLE` so host KSM can dedup identical pages across VMs — the flag persists through snapshot/clone/restore (it lives in the snapshot's CH config, not the CLI), so build the golden with it or rebuild. cocoon only sets the madvise: enabling and tuning the scanner (`/sys/kernel/mm/ksm/run`, `pages_to_scan`) is the operator's. Excludes `--hugepages`/`--shared-memory` (KSM merges only plain private pages); mmap-cloned siblings already share untouched pages via the page cache, so KSM's gain is dirtied-but-equal and cross-golden pages — measure density on your fleet, and weigh ksmd CPU plus the cross-VM dedup timing side channel in multi-tenant setups
- **Disk I/O**: multi-queue virtio-blk; readonly base disks keep host page cache (`direct=off`), writable raw COW and data disks use O_DIRECT (`direct=on`) to avoid host cache buildup and guest flush storms, and qcow2 overlays stay buffered — Cloud Hypervisor applies the disk's `direct` flag to the backing file too, and O_DIRECT there would give every VM its own read of the shared base instead of one page-cache copy
- **Balloon**: 25% of memory auto-returned via virtio-balloon with deflate-on-OOM and free-page reporting (VMs with < 256 MiB memory skip balloon)
- **Watchdog**: hardware watchdog enabled by default for automatic guest reset on hang; `--no-watchdog` is an explicit compatibility opt-out for guests whose watchdog driver is unsafe during reboot

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
