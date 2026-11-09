# Snapshots, Clone & Restore

Capture running VMs; clone them into new identities; restore or hibernate in place; move snapshots between hosts.

## Overview

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
- **Memory**: complete RAM contents (`memory-range-*` on Cloud Hypervisor, `mem` on Firecracker)
- **Disks**: COW disk (raw or qcow2), every `--data-disk`, cidata disk (cloudimg)
- **Config**: Cloud Hypervisor `config.json` plus device state (`state.json`), or Firecracker `vmstate`; both carry cocoon's `cocoon.json` sidecar (disk roles, boot config)
- **Metadata**: image reference, hypervisor type, network/queue settings, and resource topology (CPU, memory, storage, NIC count) — CPU/memory/storage are fixed at snapshot time on every backend; NIC count inherits by default and can be overridden at clone time via `--nics N` (Cloud Hypervisor, or Firecracker `--pci` snapshots — see Clone Constraints)

`snapshot save` and `vm hibernate` are refused while a vhost-user-fs share, a VFIO device or a hot-attached disk is present; see [Devices](devices.md).

On Cloud Hypervisor, `snapshot save` and `vm hibernate` first let a running guest finish inflating its balloon, waiting up to a minute and moving on once inflation stalls for 2 s (the wait reads the live target, so it covers a target raised by `vm balloon`), so a `vm restore` or hibernate wake comes back with a settled balloon instead of inflating it again at a high CPU cost. A clone moves to the create-time balloon target after restore: it deflates from a raised snapshot target, so a golden saved at the default target keeps the settled balloon.

### Clone Constraints

CPU, memory, and storage are fixed at snapshot time on both backends: the guest is reconstructed from the snapshot's binary device state, so `vm clone` and `vm restore` do not accept `--cpu`, `--memory` or `--storage` at all. NIC count inherits by default; Cloud Hypervisor clones can override it via `--nics N` (cocoon hot-swaps the snapshot's NICs for a fresh set right after restore). Firecracker clones inherit the virtio transport with the snapshot: an MMIO clone must keep the snapshot's NIC topology (`network_overrides` only retargets existing interfaces) and rejects `--data-disk`, while a `--pci` clone restores the snapshot's NICs, then hot-plugs the `--nics` delta and any `--data-disk` after restore and prints the guest-side rescan. On Firecracker, the target network's MTU must equal the snapshot's, because the guest keeps the MTU the snapshot advertised (snapshots taken before `nic_mtus` was recorded skip this check); Cloud Hypervisor clones swap in fresh NICs after restore. Fresh data disks can be added to a Cloud Hypervisor clone via `--data-disk` (hot-added after restore). Create a fresh VM with `cocoon vm run` if a different CPU/memory/storage shape is needed.

### Memory Restore Modes & Concurrency

Cloud Hypervisor clones and restores load guest memory via `--restore-mode`:

- **`mmap`** (default for plain private-anon snapshots): maps the snapshot's memory file copy-on-write — no upfront copy, and sibling clones of one snapshot share page cache for clean pages. Requires Cloud Hypervisor v54 or newer (`cocoon-check --upgrade` installs the cocoonstack fork build).
- **`copy`** (the fallback for clones and restores whenever the snapshot uses hugepages or shared memory): eager full-memory load; an explicit `mmap` degrades to `copy` with a warning, while an explicit `ondemand` is passed through unchanged.
- **`ondemand`**: userfaultfd paging; pages load on first guest access.

Firecracker restore is always memory-mapped; `--restore-mode` is accepted but ignored there.

Concurrent clones are first-class: sibling clones of one snapshot run fully in parallel, and each clone holds a shared lease on its snapshot for the duration of setup, so a concurrent `snapshot rm` fails fast with `snapshot <id> is in use by an active clone/restore/export` and a GC sweep skips it until its next cycle, instead of destroying work in flight; Firecracker clones also hold a shared per-VM lease on a managed source, so `vm rm`/`vm restore` of the source waits too. A source that was already deleted still clones (its drives travel inside the snapshot).

### Post-Clone Guest Setup

After cloning, the guest resumes with new NICs — Cloud Hypervisor clones hot-swap in fresh MAC addresses automatically, while a Firecracker clone keeps the source VM's MACs in the restored vmstate and `cocoon vm clone` prints the `ip link set dev ethN down` / `address <MAC>` / `up` triple to run first — but the guest OS still has the old IP configuration. You must reconfigure networking inside the guest: `cocoon vm clone` prints the exact steps for that VM — a `--no-balloon` clone has no balloon to release, so it gets no `drop_caches` line.

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

# Clean old network configs from snapshot, then set the hostname
rm -f /etc/systemd/network/10-*.network
echo <VM_NAME> > /etc/hostname && hostname <VM_NAME>

# Write new MAC-based configs
# (cocoon vm clone prints a ready-to-paste loop with actual MAC/IP/GW values)
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

The `cocoon vm clone` command prints these hints with the actual values after a successful clone; the `gws` lines appear only when a NIC has a gateway, DHCP-only NICs (a `--bridge` clone) get a separate block writing `DHCP=ipv4` units, and a Windows guest gets a `Get-PnpDevice` rebind plus `netsh interface ipv4 set address` instead.

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

The archive contains the snapshot config, VM config, COW disk, memory ranges, and device state — sparse files carry cocoon's own pax records so their holes survive the round-trip — everything needed to reconstruct the snapshot on a different machine. The envelope also lists the files the archive carries, and `snapshot import` refuses an archive that ends before every listed file has landed — a plain tar cut at an entry boundary otherwise reads as complete. `snapshot export --to-dir` writes the same files as a directory that `vm clone --from-dir`, `vm restore --from-dir` and `vm disk attach --from-dir` (a copy of the COW disk on a running VM of the same image; see [Devices](devices.md)) consume without a tar round-trip. Do not repack an export with a third-party tar: dropping cocoon's sparse records can rebuild a short, shifted disk. `--from-dir` and `snapshot import` reject a `cow.raw` whose size differs from the envelope's recorded storage size.

#### Cross-Node Clone

When cloning an imported snapshot on a node that does not have the original base image, use `--pull` to auto-pull it:

```bash
# On the target node: import snapshot and clone with auto-pull
cocoon snapshot import my-snap.tar.gz --name my-snap
cocoon vm clone --pull my-snap
```

The `--pull` flag uses the image digest recorded at snapshot time: OCI images are pulled by digest, so a moved tag cannot be silently substituted; for cloud images a digest mismatch is logged and the clone then fails.

**Note:** `--pull` only works for registry-pulled images (OCI and cloudimg). For imported images (local qcow2/tar files), the base image must be transferred manually to the target node before cloning.

### Restore

Restore reverts a VM — running, stopped or error — to a previous snapshot's state in-place:

```bash
# Restore a VM to a previous snapshot
cocoon vm restore my-vm my-snap
```

A restore from a local snapshot store stops the VM and populates its run dir in place, quarantining the run dir if the population fails part-way so the VM cannot boot mixed-vintage files; a streamed restore stages the snapshot into a scratch directory first and (re)starts the hypervisor only after the full extraction succeeds, so a truncated or corrupt stream errors out with the VM in its prior state. Network is fully preserved — same IP, same MAC, same network namespace; restore always runs the same network self-heal as `vm start`, so a hibernated VM resumes even after a host reboot. No guest-side reconfiguration is needed (unlike clone).

### Hibernate

Hibernate atomically snapshots a running VM and stops it, releasing its memory; resume with `vm restore`:

```bash
cocoon vm hibernate my-vm --name nap
cocoon vm restore my-vm nap
```

Pause, capture, persist, and VMM termination share one pause window: the snapshot point and the stop coincide (nothing the guest does can be lost in between), and the VMM dies only after the snapshot is durably saved — if saving fails (disk full, snapshot DB error), the VM simply resumes running and the command fails. Restore of the hibernated VM preserves machine identity (entropy-only reseed): running processes, sessions, and tmpfs contents continue where they stopped. The hibernate snapshot outlives the restore and keeps its `--name` and disk space until `snapshot rm`; `vm start` on a hibernated VM cold-boots from disk and leaves the snapshot unused.

A canceled or timed-out `vm hibernate` or `snapshot save` waits for the hypervisor to end its memory dump, up to 10 minutes, then resumes the guest, so SIGTERM can take that long to end the command. SIGKILL skips the resume; see [known issues](known-issues.md#a-killed-capture-leaves-the-guest-paused).

### Live Migration

`vm migrate` captures a running VM in rounds and restores it on another node (Cloud Hypervisor only). The guest stops only for the last round, the copy of its files and the restore, and its processes and memory carry over. Cocoon captures and restores; moving the files, choosing how many rounds to run, and deciding which node owns the VM stay with the caller.

```bash
# Source: a full baseline, then deltas. The guest runs again after each one.
cocoon vm migrate capture my-vm --output json                        # prints series, dir, files
cocoon vm migrate capture my-vm --series 123 --output json           # optional, repeatable
# Source: the last delta leaves the guest paused.
cocoon vm migrate capture my-vm --series 123 --final --output json

# Target: after copying every file each round listed into one directory
cocoon vm migrate receive --from-dir /xfer/123 --id <source-vm-id> --name my-vm --pull
cocoon vm start my-vm                                                # resumes the guest

# Source: only after the target runs
cocoon vm rm --force my-vm
```

To give up at any point before that, run `cocoon vm migrate abort my-vm --series 123`: it ends the series, resumes the guest and deletes the directory. Once the directory is gone, a repeat or an old series ID does nothing, so it cannot resume a guest paused for a newer series. After `--final`, receive the series or abort it: resuming the guest with `vm start` and capturing again leaves a delta that misses blocks the guest discarded.

Requirements:

- **Hypervisor build**: the cocoonstack fork at `35b098c65260` or newer. An older VMM answers 404 on the first capture and nothing changes. A VM keeps the VMM it started on, so it needs a restart after an upgrade.
- **Filesystem**: the source run dir needs reflink, because disk deltas come from comparing extents the disk shares with a reflinked copy. XFS is the only filesystem tested. btrfs and bcachefs pass the capture check but are untested. ext4 and tmpfs have no reflink, so capture refuses before it pauses the guest. A target run dir without reflink falls back to a full disk copy (untested), and the VM then cannot migrate away from it. On the target, keep the copied directory on the filesystem of the run dir, so memory files hard-link and the directory can go after the receive.
- **Devices**: a hot-attached disk, share or VFIO device refuses the capture, as it does for `snapshot save`.
- **Root dir**: the target's `root_dir` must be the same path as the source's. Otherwise the receive fails during the pause with `untrusted storage path in snapshot metadata`.

Rules for the caller:

- Copy every file a round lists, in the listed round order. `state.json` and `config.json` are replaced each round. A `.ref` file never leaves the source. A short or missing disk delta fails the receive.
- A VM has one series. A new baseline ends the previous one and deletes its directory; the directory of a series nobody continues stays until `gc` removes it after 24 hours. A delta with an old series ID fails and leaves the guest alone.
- A failed round ends the series, resumes the guest and deletes its directory; start again with a new baseline. A capture killed mid-round makes the next round fail the same way; a gap in `round` means a capture died after it finished, so copy every non-`.ref` file of the directory or start again. `snapshot save`, removing a NIC, a device detach, a reboot or a stop ends the series too, and the next delta fails.
- A VM cloned or received seconds ago may still eject its old NICs. A capture waits up to 30 seconds for that, then goes on with a warning; if an eject lands during a series, the next delta fails, so start again.
- Pull the image on the target before `--final`. A receive that has to pull does it during the pause.
- The receive uses the source's CNI network name. When the target has no conflist of that name, pass `--network`.
- Once the target runs, remove the source with `vm rm --force`; never `vm migrate abort` it, even when the target cold-booted, or the same guest runs twice.
- Without `--id` the receive generates an ID. With `--id`, a retry after a killed receive reclaims what it left. A retry after a finished receive fails with `id collision`; `vm inspect` shows the VM. Only `vm migrate receive` reads a series directory: `vm clone --from-dir` would pair the last memory with the baseline disks.

The received VM gets fresh NICs like a clone, with new MACs, so the guest needs the [post-clone network setup](#post-clone-guest-setup). No reseed runs: the guest state is the same one, not a copy. If the target VMM dies before `vm start`, the start cold-boots from the disk and still succeeds, so check the guest before removing the source. `--restore-mode ondemand` is rejected by the hypervisor for a series with deltas. The replayed disk ranges are still dirty in the target's page cache, so the guest's first flush after `vm start`, usually a journal commit about 5 s in, waits for them (about 3 s for 36k ranges). Metering opens the new intervals with reason `migrate`.

A qcow2 overlay is open when captured, so its dirty bit is set and the target VMM rebuilds the refcounts when it opens the file. The overlay hash therefore differs after the receive; compare it before the VMM opens it.

### Restore Constraints

- **VM must be running, stopped, or error.** Restore cold-spawns a fresh hypervisor process from the snapshot; a stopped target (e.g. hibernated) gets the `vm start` network self-heal first. Error-state VMs are admitted because restore rebuilds the run dir — it is the recovery path for a crashed or interrupted restore (which `vm start` refuses).
- **Snapshot ownership.** Only snapshots created from the same VM (tracked in `snapshot_ids`) are normally accepted; `--force` with `--from-dir` skips this ownership check only.
- **Keep the same base-image lineage.** Restore retains the target's base-image paths, boot configuration, and image references. `--force` cannot bypass the base-image check: a different image backend or resolved image digest is rejected before stopping the VM or overwriting its disks. Use `vm clone` for another base image. Both backends check NIC MACs (Firecracker from the `nic_macs` field its snapshot sidecar records); Firecracker also requires the snapshot's recorded COW and data-disk paths to match the target.
- **CPU, memory, and storage come from the snapshot.** The hypervisor reconstructs the guest from snapshot state, so these are not configurable at restore time; cocoon realigns the persisted record to match.
- **NIC count and MACs must match the target VM.** Restore reuses the VM's existing network namespace, TAP devices, and IP allocation; a mismatched count is rejected, and so is a NIC whose MAC changed since the snapshot (`vm net` down and back up), because the guest would resume on the old MAC. A Firecracker snapshot from an older cocoon records no MACs, so only its count is checked.
