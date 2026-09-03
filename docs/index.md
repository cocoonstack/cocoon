# cocoon

Lightweight MicroVM engine with dual hypervisor backends:
[Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor)
(default) and
[Firecracker](https://github.com/firecracker-microvm/firecracker).
One hypervisor process per VM, every command standalone (a resident daemon
is optional); Docker-like CLI; snapshots that clone into new identities in
tens of milliseconds.

```
cocoon CLI ──► images: OCI (EROFS layers, direct boot) | cloudimg (qcow2, UEFI)
           ──► vm: create/run/console/exec ── CH or FC, COW disk, CNI/bridge NIC
           ──► snapshot: save/clone/restore/hibernate ── export/import across hosts
```

## Guides

- [Installation](install.md) — requirements, releases, the doctor script,
  quick start, shell completion, building from source
- [CLI reference](cli.md) — the full command tree and every flag
- [Images](images.md) — pulling OCI and cloud images, importing local
  files, the official image catalog
- [VM lifecycle](vm.md) — states, shutdown behavior, cloud-init first
  boot, data disks, performance tuning, live status
- [Networking](networking.md) — CNI with TC redirect, bridge mode,
  multi-NIC, NIC hot-resize
- [Snapshots, clone & restore](snapshots.md) — capture, clone, restore,
  hibernate, export/import, cross-node moves
- [Runtime device attach](devices.md) — vhost-user-fs shares, VFIO PCI
  passthrough, external disk hot-attach
- [Windows guests](windows.md) — the `--windows` path and the patched
  CH/firmware forks it needs
- [Firecracker backend](firecracker.md) — what `--fc` trades for ~125ms
  boots and <5 MiB overhead
- [Garbage collection](gc.md) — cross-module GC and snapshot LRU eviction
- [Daemon](daemon.md) — the optional resident supervisor: crash convergence,
  transition tracking, read-only API
- [OS images](os-image.md) — the official Ubuntu/Android/Windows images
  and the build harness
- [Known issues](known-issues.md) — limitations, workarounds, upstream
  tracking

## Features

- **OCI VM images** — pull OCI images with kernel + rootfs layers, content-addressed blob cache with SHA-256 deduplication
- **Cloud image support** — pull from HTTP/HTTPS URLs (e.g. Ubuntu cloud images), automatic qcow2 conversion
- **Image import** — import local qcow2 or tar files (also from stdin or gzip-wrapped streams), auto-detected by magic bytes
- **UEFI boot** — CLOUDHV.fd firmware by default; direct kernel boot for OCI images (auto-detected)
- **COW overlays** — copy-on-write disks backed by shared base images (raw for OCI, qcow2 for cloud images)
- **CNI networking** — automatic NIC creation via CNI plugins, multi-NIC support, per-VM IP allocation; bridge mode and NIC hot-resize; `net_scope` keys host device names per installation so co-hosted cocoon roots never GC each other's guests
- **CPU isolation** — every VM runs in its own cgroup v2 scope with Guaranteed-at-N defaults (`--cpu` caps the long-run average); raw weight/quota/burst knobs, an optional host-core fence (`cgroup_cpus`), and per-VM pinning (`--cpuset-cpus`); see [CPU Isolation](vm.md#cpu-isolation-cgroup-v2)
- **Multi-queue virtio-net** — TAP devices created with per-vCPU queue pairs; configurable ring depth (`--queue-size`, default 512); TSO/UFO/csum offload enabled by default
- **TC redirect I/O path** — veth ↔ TAP wired via ingress qdisc + mirred redirect (no bridge in the data path)
- **DNS configuration** — custom DNS servers injected into VMs via kernel cmdline (OCI) or cloud-init network-config (cloudimg)
- **Cloud-init metadata** — automatic NoCloud cidata FAT12 disk for cloudimg VMs (hostname, configurable user/password via `--user`/`--password`, multi-NIC Netplan v2 network-config); cidata is automatically skipped on subsequent boots
- **User data disks** — `--data-disk` attaches additional virtio-blk disks per VM, with optional ext4 mkfs at create time, cloud-init `mounts:` auto-mount on cloudimg+CH (via `/dev/disk/by-id/virtio-<name>`), per-disk DirectIO override, and 1:1 inheritance through snapshot/clone/restore; `vm clone --data-disk` adds fresh disks to a clone and `vm disk attach/detach` hot-plugs existing raw files on a running VM (both CH only)
- **Copy-on-write clone restore** — Cloud Hypervisor clones of plain private-anon snapshots default to `mmap` memory restore: no eager copy, page cache shared across sibling clones; hugepages/shared snapshots fall back to eager copy with a warning
- **Hugepages** — opt-in via `vm create --hugepages` (Cloud Hypervisor only); backs VM memory with hugetlbfs at the cost of the mmap restore fast path for that VM's snapshots (never supported on Firecracker, whose snapshots cannot restore from hugetlbfs)
- **Mergeable memory** — opt-in `--mergeable` (Cloud Hypervisor only) madvises guest memory `MADV_MERGEABLE` so host KSM can dedup identical pages across VMs; excludes `--hugepages`/`--shared-memory`
- **Memory balloon** — 25% of memory returned via virtio-balloon (deflate-on-OOM, free-page reporting) when memory >= 256 MiB
- **Graceful shutdown** — ACPI power-button for UEFI VMs with configurable timeout, fallback to SIGTERM → SIGKILL
- **Interactive console** — `cocoon vm console` with bidirectional PTY relay, SSH-style escape sequences (`~.` disconnect, `~?` help), configurable escape character, SIGWINCH propagation
- **Snapshot & clone** — `cocoon snapshot save` captures a running VM's full state (memory, disks, config); `cocoon vm clone` restores it as a new VM with fresh network and identity; guest resources (CPU, memory, storage, NIC count) inherit verbatim from the snapshot, while host-side cgroup CPU policy comes from clone flags (see [CPU Isolation](vm.md#cpu-isolation-cgroup-v2))
- **Snapshot export & import** — `cocoon snapshot export` packages a snapshot into a portable `.tar` archive (`.tar.gz` with `--gzip`, sparse-aware pax headers); `cocoon snapshot import` restores it on another host or cluster; supports piping via stdout/stdin for direct host-to-host transfer; `--to-dir` writes a directory form (with `snapshot.json` envelope) for NFS / rsync-friendly handoff
- **Clone / restore from a directory** — `cocoon vm clone --from-dir DIR` and `cocoon vm restore --from-dir DIR` consume any directory containing a `snapshot.json` envelope without first registering the snapshot in the local DB; the dir is treated as read-only so multi-VM golden-image use cases work without copying
- **Live status monitoring** — `cocoon vm status` watches VM state changes in real time via fsnotify, with refresh mode (top-like) and event-stream mode (append-only, for scripting and vk-cocoon integration)
- **Docker-like CLI** — `create`, `run`, `start`, `stop`, `list`, `inspect`, `console`, `rm`, `debug`, `clone`, `status`
- **Structured logging** — configurable log level (`--log-level`); logs go to stderr until `log.filename` is set, which is also what enables rotation via `log.maxsize` / `log.maxage` / `log.maxbackups` (500 MB / 28 days / 3 files by default)
- **Debug command** — `cocoon vm debug` generates a copy-pasteable `cloud-hypervisor` command for manual debugging
- **Firecracker backend** — `--fc` flag selects Firecracker for OCI images: ~125ms boot, <5 MiB overhead, minimal attack surface (no UEFI, no qcow2, no Windows)
- **Daemon-optional architecture** — one hypervisor process per VM, every command standalone; `cocoon daemon` optionally adopts running VMs to converge crashes as they happen
- **Garbage collection** — modular lock-safe GC with cross-module snapshot resolution; protects blobs referenced by running VMs and snapshots
- **Doctor script** — pre-flight environment check and one-command dependency installation


## Repository

Source and issue tracker:
[github.com/cocoonstack/cocoon](https://github.com/cocoonstack/cocoon).
Part of the [cocoonstack](https://cocoonstack.github.io/) MicroVM platform.
