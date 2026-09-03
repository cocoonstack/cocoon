# Snapshots, Clone & Restore

Capture running VMs; clone them into new identities; restore or hibernate in place; move snapshots between hosts.

## Overview

Cocoon supports snapshotting a running VM and cloning it into one or more new VMs.

### Workflow

```bash
# 1. Snapshot a running VM
cocoon snapshot save --name my-snap my-vm

# 2. List snapshots
cocoon snapshot list

# 3. Clone a new VM from the snapshot
cocoon vm clone my-snap

# 4. Clone with a custom name
cocoon vm clone --name fresh-clone my-snap

# 5. Delete a snapshot
cocoon snapshot rm my-snap
```

### What Gets Captured

A snapshot contains the full VM state:
- **Memory**: complete RAM contents (memory-ranges)
- **Disks**: COW disk (raw or qcow2), cidata disk (cloudimg)
- **Config**: Cloud Hypervisor config.json and device state (state.json)
- **Metadata**: image reference, hypervisor type, network/queue settings, and resource topology (CPU, memory, storage, NIC count) — CPU/memory/storage are fixed at snapshot time on every backend; NIC count inherits by default and can be overridden at clone time via `--nics N` (Cloud Hypervisor only — see Clone Constraints)

### Clone Constraints

CPU, memory, and storage are fixed at snapshot time on both backends: the guest is reconstructed from the snapshot's binary device state, so growing them at clone time would not be honored. NIC count inherits by default; Cloud Hypervisor clones can override it via `--nics N` (cocoon hot-swaps the snapshot's NICs for a fresh set right after restore). Firecracker clones must keep the snapshot's NIC topology — `--nics` is rejected because FC's `network_overrides` only retargets existing interfaces, it cannot add or drop them. Fresh data disks can be added to a Cloud Hypervisor clone via `--data-disk` (hot-added after restore); Firecracker rejects it — no hotplug. Create a fresh VM with `cocoon vm run` if a different CPU/memory/storage shape is needed.

### Memory Restore Modes & Concurrency

Cloud Hypervisor clones restore guest memory via `--restore-mode`:

- **`mmap`** (default for clones of plain private-anon snapshots): maps the snapshot's memory file copy-on-write — no upfront copy, and sibling clones of one snapshot share page cache for clean pages. Requires the cocoonstack CH build.
- **`copy`** (the fallback for clones and restores whenever the snapshot uses hugepages or shared memory): eager full-memory load; the degradation from an explicit `mmap` request is logged as a warning.
- **`ondemand`**: userfaultfd paging; pages load on first guest access.

Firecracker restore is always memory-mapped by design and takes no mode flag.

Concurrent clones are first-class: sibling clones of one snapshot run fully in parallel, and each clone holds a shared per-VM lease on its source for the duration of setup — a concurrent `vm rm`/`vm restore` of the source waits for the burst to finish instead of destroying work in flight. A source that was already deleted still clones (its drives travel inside the snapshot).

### Post-Clone Guest Setup

After cloning, the guest resumes with new NICs (MAC addresses are handled automatically via NIC hot-swap during clone), but the guest OS still has the old IP configuration. You must reconfigure networking inside the guest:

**Cloudimg VMs** (cloud-init re-initialization):

```bash
# Release balloon memory (the snapshot's memory pages are still cached)
echo 3 > /proc/sys/vm/drop_caches

# Clean old network configs from snapshot and reconfigure via cloud-init
rm -f /etc/systemd/network/10-*.network
cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init
cloud-init modules --mode=config && systemctl restart systemd-networkd
```

**OCI VMs** (MAC-based systemd-networkd reconfiguration — the new values are printed by `cocoon vm clone`):

```bash
# Release balloon memory
echo 3 > /proc/sys/vm/drop_caches

# Set hostname
hostnamectl set-hostname <VM_NAME>

# Clean old network configs from snapshot and write new ones (MAC-based)
# (cocoon vm clone prints a ready-to-paste loop with actual MAC/IP/GW values)
rm -f /etc/systemd/network/10-*.network
macs=('<MAC0>' '<MAC1>')
addrs=('<NEW_IP0>/<PREFIX>' '<NEW_IP1>/<PREFIX>')
gws=('<GATEWAY0>' '<GATEWAY1>')
for i in "${!macs[@]}"; do
  f="/etc/systemd/network/10-${macs[$i]//:/}.network"
  printf '[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s\n' "${macs[$i]}" "${addrs[$i]}" > "$f"
  [ -n "${gws[$i]}" ] && printf 'Gateway=%s\n' "${gws[$i]}" >> "$f"
done
systemctl restart systemd-networkd
```

The `cocoon vm clone` command prints these hints with the actual values after a successful clone.

### Export & Import

Snapshots can be exported to portable tar archives (gzip with `--gzip`) for transfer between hosts or clusters, and imported back:

```bash
# Export a snapshot to a file
cocoon snapshot export my-snap --gzip -o my-snap.tar.gz

# Import on another host
cocoon snapshot import my-snap.tar.gz --name imported-snap

# Clone from the imported snapshot
cocoon vm clone imported-snap

# Or pipe directly between hosts (no intermediate file)
cocoon snapshot export my-snap -o - | ssh host2 cocoon snapshot import --name my-snap
```

The archive contains the snapshot config, VM config, COW disk (with sparse-aware pax headers for efficient compression), memory ranges, and device state — everything needed to reconstruct the snapshot on a different machine.

#### Cross-Node Clone

When cloning an imported snapshot on a node that does not have the original base image, use `--pull` to auto-pull it:

```bash
# On the target node: import snapshot and clone with auto-pull
cocoon snapshot import my-snap.tar.gz --name my-snap
cocoon vm clone --pull my-snap
```

The `--pull` flag uses the image digest recorded at snapshot time to verify that the pulled image matches the exact version the snapshot was created from. If the tag has been updated since the snapshot was taken, a warning is logged.

**Note:** `--pull` only works for registry-pulled images (OCI and cloudimg). For imported images (local qcow2/tar files), the base image must be transferred manually to the target node before cloning.

### Restore

Restore reverts a VM — running or stopped — to a previous snapshot's state in-place:

```bash
# Restore a VM to a previous snapshot
cocoon vm restore my-vm my-snap
```

Cocoon stages the snapshot into a scratch directory first, then (re)starts the hypervisor process only after the full extraction succeeds — a truncated or corrupt snapshot stream errors out with the VM in its prior state. Network is fully preserved — same IP, same MAC, same network namespace; restoring a stopped VM first runs the same network self-heal as `vm start`, so a hibernated VM resumes even after a host reboot. No guest-side reconfiguration is needed (unlike clone).

### Hibernate

Hibernate atomically snapshots a running VM and stops it, releasing its memory; resume with `vm restore`:

```bash
cocoon vm hibernate my-vm --name nap
cocoon vm restore my-vm nap
```

Pause, capture, persist, and VMM termination share one pause window: the snapshot point and the stop coincide (nothing the guest does can be lost in between), and the VMM dies only after the snapshot is durably saved — if saving fails (disk full, snapshot DB error), the VM simply resumes running and the command fails. Restore of the hibernated VM preserves machine identity (entropy-only reseed): running processes, sessions, and tmpfs contents continue where they stopped.

### Restore Constraints

- **VM must be running, stopped, or error.** Restore cold-spawns a fresh hypervisor process from the snapshot; a stopped target (e.g. hibernated) gets the `vm start` network self-heal first. Error-state VMs are admitted because restore rebuilds the run dir — it is the recovery path for a crashed or interrupted restore (which `vm start` refuses).
- **Snapshot must belong to the VM.** Only snapshots created from the same VM (tracked in `snapshot_ids`) are accepted; pass `--force` with `--from-dir` to opt into a foreign lineage.
- **CPU, memory, and storage come from the snapshot.** The hypervisor reconstructs the guest from snapshot state, so these are not configurable at restore time; cocoon realigns the persisted record to match.
- **NIC count must match the target VM.** Restore reuses the VM's existing network namespace, TAP devices, and IP allocation; a mismatched count is rejected.
