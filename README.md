# Cocoon

Lightweight MicroVM engine with dual hypervisor backends: [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) (default) and [Firecracker](https://github.com/firecracker-microvm/firecracker).

## Features

- **OCI VM images** — pull OCI images with kernel + rootfs layers, content-addressed blob cache with SHA-256 deduplication
- **Cloud image support** — pull from HTTP/HTTPS URLs (e.g. Ubuntu cloud images), automatic qcow2 conversion
- **Image import** — import local qcow2 or tar files (also from stdin or gzip-wrapped streams), auto-detected by magic bytes
- **UEFI boot** — CLOUDHV.fd firmware by default; direct kernel boot for OCI images (auto-detected)
- **COW overlays** — copy-on-write disks backed by shared base images (raw for OCI, qcow2 for cloud images)
- **CNI networking** — automatic NIC creation via CNI plugins, multi-NIC support, per-VM IP allocation
- **Multi-queue virtio-net** — TAP devices created with per-vCPU queue pairs; configurable ring depth (`--queue-size`, default 512); TSO/UFO/csum offload enabled by default
- **TC redirect I/O path** — veth ↔ TAP wired via ingress qdisc + mirred redirect (no bridge in the data path)
- **DNS configuration** — custom DNS servers injected into VMs via kernel cmdline (OCI) or cloud-init network-config (cloudimg)
- **Cloud-init metadata** — automatic NoCloud cidata FAT12 disk for cloudimg VMs (hostname, configurable user/password via `--user`/`--password`, multi-NIC Netplan v2 network-config); cidata is automatically skipped on subsequent boots
- **User data disks** — `--data-disk` attaches additional virtio-blk disks per VM, with optional ext4 mkfs at create time, cloud-init `mounts:` auto-mount on cloudimg+CH (via `/dev/disk/by-id/virtio-<name>`), per-disk DirectIO override, and 1:1 inheritance through snapshot/clone/restore; `vm clone --data-disk` adds fresh disks to a clone and `vm disk attach/detach` hot-plugs existing raw files on a running VM (both CH only)
- **Hugepages** — automatic detection of host hugepage configuration; VM memory backed by hugepages when available
- **Memory balloon** — 25% of memory returned via virtio-balloon (deflate-on-OOM, free-page reporting) when memory >= 256 MiB
- **Graceful shutdown** — ACPI power-button for UEFI VMs with configurable timeout, fallback to SIGTERM → SIGKILL
- **Interactive console** — `cocoon vm console` with bidirectional PTY relay, SSH-style escape sequences (`~.` disconnect, `~?` help), configurable escape character, SIGWINCH propagation
- **Snapshot & clone** — `cocoon snapshot save` captures a running VM's full state (memory, disks, config); `cocoon vm clone` restores it as a new VM with fresh network and identity; all resources (CPU, memory, storage, NIC count) inherit verbatim from the snapshot
- **Snapshot export & import** — `cocoon snapshot export` packages a snapshot into a portable `.tar.gz` archive (with sparse-aware pax headers); `cocoon snapshot import` restores it on another host or cluster; supports piping via stdout/stdin for direct host-to-host transfer; `--to-dir` writes a directory form (with `snapshot.json` envelope) for NFS / rsync-friendly handoff
- **Clone / restore from a directory** — `cocoon vm clone --from-dir DIR` and `cocoon vm restore --from-dir DIR` consume any directory containing a `snapshot.json` envelope without first registering the snapshot in the local DB; the dir is treated as read-only so multi-VM golden-image use cases work without copying
- **Live status monitoring** — `cocoon vm status` watches VM state changes in real time via fsnotify, with refresh mode (top-like) and event-stream mode (append-only, for scripting and vk-cocoon integration)
- **Docker-like CLI** — `create`, `run`, `start`, `stop`, `list`, `inspect`, `console`, `rm`, `debug`, `clone`, `status`
- **Structured logging** — configurable log level (`--log-level`), log rotation (max size / age / backups)
- **Debug command** — `cocoon vm debug` generates a copy-pasteable `cloud-hypervisor` command for manual debugging
- **Firecracker backend** — `--fc` flag selects Firecracker for OCI images: ~125ms boot, <5 MiB overhead, minimal attack surface (no UEFI, no qcow2, no Windows)
- **Zero-daemon architecture** — one hypervisor process per VM, no long-running daemon
- **Garbage collection** — modular lock-safe GC with cross-module snapshot resolution; protects blobs referenced by running VMs and snapshots
- **Doctor script** — pre-flight environment check and one-command dependency installation

## Requirements

- Linux with KVM (x86_64 or aarch64)
- Root access (sudo)
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v51.0+ (for Windows VMs, use our [CH fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) and [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) for full compatibility — see [KNOWN_ISSUES.md](KNOWN_ISSUES.md))
- [Firecracker](https://github.com/firecracker-microvm/firecracker) v1.16+ (optional, for `--fc` backend; `vm clone` requires >= v1.16 for the vsock override)
- `qemu-img` (from qemu-utils, for cloud images)
- `mkfs.erofs` from erofs-utils **>= 1.8** (for OCI images; 1.7.x tar mode
  silently corrupts layers — cocoon refuses to convert with older versions)
- UEFI firmware (`CLOUDHV.fd`, for cloud images, not needed with `--fc`)
- CNI plugins (`bridge`, `host-local`, `loopback`)
- Go 1.25+ (build only)

## Installation

### GitHub Releases

Download pre-built binaries from [GitHub Releases](https://github.com/cocoonstack/cocoon/releases):

```bash
# Linux amd64
curl -fsSL -o cocoon.tar.gz https://github.com/cocoonstack/cocoon/releases/download/v0.4.8/cocoon_0.4.8_Linux_x86_64.tar.gz
tar -xzf cocoon.tar.gz
install -m 0755 cocoon /usr/local/bin/

# Or use go install
go install github.com/cocoonstack/cocoon@latest
```

### Build from source

```bash
git clone https://github.com/cocoonstack/cocoon.git
cd cocoon
make build
```

This produces a `cocoon` binary in the project root. Use `make install` to install into `$GOPATH/bin`.

## Doctor

Cocoon ships a diagnostic script that checks your environment and can auto-install all dependencies:

```bash
# Get script
curl -fsSL -o cocoon-check https://raw.githubusercontent.com/cocoonstack/cocoon/refs/heads/master/doctor/check.sh
install -m 0755 cocoon-check /usr/local/bin/

# Check only — reports PASS/FAIL for each requirement
cocoon-check

# Check and fix — creates directories, sets sysctl, adds iptables rules
cocoon-check --fix

# Full setup — install cloud-hypervisor, firmware, and CNI plugins
cocoon-check --upgrade
```

The `--upgrade` flag downloads and installs:
- Cloud Hypervisor + ch-remote (static binaries)
- Firecracker (static binary)
- CLOUDHV.fd firmware (rust-hypervisor-firmware)
- CNI plugins (bridge, host-local, loopback, etc.)

## Quick Start

```bash
# Set up the environment (first time)
sudo cocoon-check --upgrade

# Pull an OCI VM image
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Or pull a cloud image from URL
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Create and start a VM
cocoon vm run --name my-vm --cpu 2 --memory 1G ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Attach interactive console
cocoon vm console my-vm

# List running VMs
cocoon vm list

# Stop and delete
cocoon vm stop my-vm
cocoon vm rm my-vm
```

## CLI Commands

```
cocoon
├── image
│   ├── pull [--force] IMAGE [IMAGE...]  Pull OCI image(s) or cloud image URL(s) (--force to bypass cache)
│   ├── list (alias: ls)           List locally stored images
│   ├── rm ID [ID...]              Delete locally stored image(s)
│   ├── import NAME [FILE...]      Import image from file(s) or stdin
│   └── inspect IMAGE              Show detailed image info (JSON)
├── vm
│   ├── create [flags] IMAGE       Create a VM from an image
│   ├── run [flags] IMAGE          Create and start a VM
│   ├── clone [flags] SNAPSHOT     Clone a new VM from a snapshot
│   ├── start VM [VM...]           Start created/stopped VM(s)
│   ├── stop VM [VM...]            Stop running VM(s)
│   ├── list (alias: ls)           List VMs with status
│   ├── inspect VM                 Show detailed VM info (JSON)
│   ├── console [flags] VM         Attach interactive console
│   ├── exec [flags] VM -- CMD     Run a command in a running VM via cocoon-agent (vsock)
│   ├── logs [-f] [--tail N] VM    Print the per-VM hypervisor log file
│   ├── rm [flags] VM [VM...]      Delete VM(s) (--force kills running VMs immediately)
│   ├── restore [flags] VM SNAP   Restore a VM (running or stopped) to a snapshot
│   ├── hibernate [flags] VM       Atomically snapshot a running VM and stop it
│   ├── status [VM...]             Watch VM status in real time
│   ├── fs
│   │   ├── attach [flags] VM     Attach a vhost-user-fs share (CH only)
│   │   └── detach [flags] VM     Detach a vhost-user-fs share by --tag
│   ├── device
│   │   ├── attach [flags] VM     Attach a VFIO PCI device (CH only)
│   │   └── detach [flags] VM     Detach a VFIO PCI device by --id
│   ├── disk
│   │   ├── attach [flags] VM     Hot-attach an existing raw disk file (CH only)
│   │   └── detach [flags] VM     Detach a hot-attached disk by --name (keeps the file)
│   ├── net [flags] VM             Resize NIC count on a running VM (CH only)
│   └── debug [flags] IMAGE        Generate hypervisor launch command (dry run)
├── snapshot
│   ├── save [flags] VM            Create a snapshot from a running VM
│   ├── list (alias: ls)           List all snapshots
│   ├── inspect SNAPSHOT           Show detailed snapshot info (JSON)
│   ├── rm SNAPSHOT [SNAPSHOT...]  Delete snapshot(s)
│   ├── export [flags] SNAPSHOT    Export snapshot to portable archive (or stdout)
│   └── import [flags] [FILE]      Import snapshot from archive (or stdin)
├── gc [flags]                     Remove unreferenced blobs, VM dirs; --snapshot for LRU snapshot eviction
├── version                        Show version, revision, and build time
└── completion [bash|zsh|fish|powershell]
```

## Global Flags

| Flag              | Env Variable                   | Default            | Description                            |
| ----------------- | ------------------------------ | ------------------ | -------------------------------------- |
| `--config`        |                                |                    | Config file path                       |
| `--root-dir`      | `COCOON_ROOT_DIR`              | `/var/lib/cocoon`  | Root directory for persistent data     |
| `--run-dir`       | `COCOON_RUN_DIR`               | `/var/lib/cocoon/run` | Runtime directory for sockets and PIDs |
| `--log-dir`       | `COCOON_LOG_DIR`               | `/var/log/cocoon`  | Log directory for VM and process logs  |
| `--log-level`     | `COCOON_LOG_LEVEL`             | `info`             | Log level: debug, info, warn, error    |
| `--cni-conf-dir`  | `COCOON_CNI_CONF_DIR`          | `/etc/cni/net.d`   | CNI plugin config directory            |
| `--cni-bin-dir`   | `COCOON_CNI_BIN_DIR`           | `/opt/cni/bin`     | CNI plugin binary directory            |
| `--dns`           | `COCOON_DNS`                   | `8.8.8.8,1.1.1.1`  | DNS servers for VMs (comma separated)  |

Config-file / env-only keys (no CLI flag):

| Key          | Env Variable        | Default | Description                                                            |
| ------------ | ------------------- | ------- | ---------------------------------------------------------------------- |
| `pull_conns` | `COCOON_PULL_CONNS` | `8`     | Concurrent HTTP Range connections per cloud-image download (`image pull`); raise for fat pipes, lower to be gentle on the registry |

## VM Flags

Applies to `cocoon vm create`, `cocoon vm run`, and `cocoon vm debug`:

| Flag        | Default          | Description                                   |
| ----------- | ---------------- | --------------------------------------------- |
| `--fc`      | `false`          | Use Firecracker backend (OCI images only)      |
| `--name`    | `cocoon-<image>` | VM name                                       |
| `--cpu`     | `2`              | Boot CPUs (must not exceed host core count)   |
| `--memory`  | `1G`             | Memory size (e.g., 512M, 2G)                  |
| `--storage` | `10G`            | COW disk size (e.g., 10G, 20G)                |
| `--nics`    | `1`              | Number of network interfaces (0 = no network) |
| `--queue-size` | `0` (default 512) | Virtio-net ring depth per queue (larger = better bulk throughput, smaller = better RPC latency; CH only, ignored by FC) |
| `--disk-queue-size` | `0` (default 512) | Virtio-blk ring depth per device (CH only, ignored by FC) |
| `--network` | empty (default)  | CNI conflist name (empty = first conflist)     |
| `--bridge`  | empty            | TAP-on-bridge mode (value is bridge device, e.g. `cni0`); mutually exclusive with `--network` |
| `--user`    | `root`           | Guest username for cloud-init (cloudimg only)  |
| `--password` | `cocoon`        | Guest password for cloud-init (cloudimg only)  |
| `--no-direct-io` | `false`     | Disable O_DIRECT on writable disks (use page cache; CH only, useful for dev/test with few VMs) |
| `--data-disk` | empty (repeatable) | Attach an extra data disk: `size=20G[,name=...][,fstype=ext4|none][,mount=/mnt/x][,directio=on|off|auto]`. See [Data Disks](#data-disks) |
| `--windows` | `false`          | Windows guest (UEFI boot, kvm_hyperv=on, no cidata) |
| `--shared-memory` | `false`     | Enable CH `memory shared=on`; required for later `vm fs attach` (CH only, fixed for VM lifetime) |

### Clone Flags

Applies to `cocoon vm clone`:

| Flag        | Default                  | Description                                             |
| ----------- | ------------------------ | ------------------------------------------------------- |
| `--name`    | `cocoon-clone-<id>`      | VM name                                                 |
| `--nics`    | inherit from snapshot    | Override NIC count at clone time; lets a 0-NIC snapshot clone with networking (CH hot-swaps NICs after restore) |
| `--queue-size` | `0` (inherit)         | Virtio-net ring depth per queue (0 = inherit from snapshot) |
| `--disk-queue-size` | `0` (inherit)    | Virtio-blk ring depth per device (0 = inherit from snapshot; CH only) |
| `--network` | empty (inherit)          | CNI conflist name (empty = inherit from source VM)       |
| `--bridge`  | empty                    | TAP-on-bridge mode (value is bridge device); mutually exclusive with `--network` |
| `--no-direct-io` | `false` (inherit)  | Disable O_DIRECT on writable disks (inherit from snapshot if not set) |
| `--on-demand` | `false`             | Use UFFD on-demand memory loading for faster clone (CH only; snapshot file must remain on disk) |
| `--pull`  | `false`              | Auto-pull base image if not found locally (for cross-node clone)      |
| `--from-dir` | empty                | Clone from a snapshot directory (must contain `snapshot.json`); mutually exclusive with positional `SNAPSHOT` |
| `--data-disk` | empty (repeatable)  | Create a fresh data disk for the clone and hot-add it after restore: `size=20G[,name=...][,fstype=ext4|none]` (CH only; names must not collide with disks inherited from the snapshot) |

CPU, memory, and storage all inherit from the snapshot — both hypervisors
restore the guest from the snapshot's binary device state, so those values
are fixed at snapshot time. NIC count inherits by default but `--nics N`
overrides it (CH only) by hot-swapping the snapshot's NICs for a fresh set
right after restore. Use `cocoon vm run` to create a fresh VM with different
CPU/memory/storage.

**Network backend** is decided per clone (the snapshot does not persist a
bridge device). Precedence:

1. `--bridge X` → bridge backend with bridge device `X`.
2. `--network Y` (no `--bridge`) → CNI backend with conflist `Y`.
3. neither → CNI backend, conflist inherited from the snapshot's recorded
   `vmCfg.Network` (empty = CNI default).

A bridge-backed source snapshot cloned without `--bridge` silently defaults
to CNI. Pass `--bridge X` at clone time to keep bridge mode.

### Restore Flags

Applies to `cocoon vm restore`:

| Flag          | Default | Description                                                                                            |
| ------------- | ------- | ------------------------------------------------------------------------------------------------------ |
| `--on-demand` | `false` | Use UFFD on-demand memory loading for faster restore (CH only; snapshot file must remain on disk)      |
| `--from-dir`  | empty   | Restore from a snapshot directory (must contain `snapshot.json`); mutually exclusive with positional `SNAPSHOT` |
| `--force`     | `false` | Skip the snapshot-belongs-to-VM check (only meaningful with `--from-dir`)                              |

CPU, memory, and storage come from the snapshot (the hypervisor
reconstructs the guest from snapshot state, so the persisted record is
realigned to match). NIC count must match the target VM — restore reuses
its existing network namespace, TAP devices, and IP allocation.

### Snapshot Flags

Applies to `cocoon snapshot save`:

| Flag            | Default | Description          |
| --------------- | ------- | -------------------- |
| `--name`        |         | Snapshot name        |
| `--description` |         | Snapshot description |

### Export Flags

Applies to `cocoon snapshot export`:

| Flag            | Default                    | Description                                       |
| --------------- | -------------------------- | ------------------------------------------------- |
| `--output`, `-o` |  `<name-or-id>.tar.gz`    | Output file path (`-` for stdout)                 |
| `--gzip`        | `false`                    | Compress output with gzip                         |
| `--to-dir`      |                            | Export into a directory (must be empty/absent) instead of a tar; pairs with `vm clone --from-dir` |

`--to-dir` writes a `snapshot.json` envelope alongside reflink-copied data files. Useful for NFS golden images or rsync-friendly handoff: `cocoon snapshot export snap -o ... --to-dir /nfs/golden && rsync ...`. Mutually exclusive with `--output` and `--gzip`.

### Import Flags

Applies to `cocoon snapshot import`:

| Flag            | Default | Description                    |
| --------------- | ------- | ------------------------------ |
| `--name`        |         | Override snapshot name          |
| `--description` |         | Override snapshot description   |

When FILE is omitted, data is read from stdin. This enables piping: `cocoon snapshot export snap1 -o - | ssh host2 cocoon snapshot import --name snap1`.

### Direct Clone / Restore From a Directory

`vm clone --from-dir DIR` and `vm restore --from-dir DIR` accept any directory containing a `snapshot.json` envelope (output of `snapshot export --to-dir`, or an extracted `.tar`). The snapshot does not need to be in the local snapshot DB:

```bash
# Build a portable snapshot dir, ship it, clone from it:
cocoon snapshot export my-snap --to-dir /nfs/golden
# ... rsync /nfs/golden to host B if needed ...
cocoon vm clone --from-dir /nfs/golden --name fresh-vm --pull

# Restore the same VM's externally-staged backup (envelope ID matches → silent OK):
cocoon vm restore my-vm --from-dir /sync/from-host-a

# Force-restore a foreign snapshot (acknowledges data-loss risk):
cocoon vm restore my-vm --from-dir /unrelated/lineage --force
```

The dir is read-only across the call, so multiple clones of the same dir (golden image use case) are safe. Pass `--pull` if the base image's blobs may not be present locally — `EnsureImage` reads `image_blob_ids` from the envelope and pulls as needed.

### Status Flags

Applies to `cocoon vm status`:

| Flag               | Default | Description                                             |
| ------------------ | ------- | ------------------------------------------------------- |
| `--interval`, `-n` | `5`     | Poll interval in seconds                                |
| `--event`          | `false` | Event stream mode (append changes instead of refreshing) |
| `--format`         |         | Output format: `json` (event mode only)                  |

### Debug-only Flags

Applies to `cocoon vm debug`:

| Flag        | Default              | Description                                        |
| ----------- | -------------------- | -------------------------------------------------- |
| `--max-cpu` | `8`                  | Max CPUs for the generated command                  |
| `--balloon` | `0`                  | Balloon size in MB (0 = auto)                       |
| `--cow`     |                      | COW disk path (default: auto-generated)             |
| `--ch`      | `cloud-hypervisor`   | cloud-hypervisor binary path                        |

### Console Flags

| Flag             | Default  | Description                                       |
| ---------------- | -------- | ------------------------------------------------- |
| `--escape-char`  | `^]`     | Escape character (single char or `^X` caret notation) |

### Exec Flags

`cocoon vm exec` runs a command inside a running VM via the cocoon-agent (vsock, no SSH). Stdin/stdout/stderr stream like `kubectl exec`; the host shell sees the guest command's exit code.

| Flag           | Default | Description                                        |
| -------------- | ------- | -------------------------------------------------- |
| `--env`, `-e`  |         | Extra env var `KEY=VALUE` (repeatable)              |

```
$ cocoon vm exec myvm -- uname -n
myvm
$ echo hello | cocoon vm exec myvm -- cat
hello
$ cocoon vm exec -e FOO=bar myvm -- sh -c 'echo $FOO'
bar
```

Requires cocoon-agent to be running inside the guest. All official `ghcr.io/cocoonstack/cocoon/ubuntu:*` and `ghcr.io/cocoonstack/cocoon/android:*` images now bake the binary and enable it on boot (systemd unit on Ubuntu, init.rc service on Android). The official `ghcr.io/cocoonstack/windows/win11:*` images bake cocoon-agent v0.1.8 as a Windows service via SCM; DIY Windows images need to install the agent themselves.

### Logs Flags

`cocoon vm logs` prints the per-VM hypervisor process log (`cloud-hypervisor.log` or `firecracker.log` under the configured `log_dir`). The log captures VMM-side activity — device init warnings, API errors, virtio messages, shutdown — but **not** guest console output (use `cocoon vm console` for that). The file lives under the VM's log dir for as long as the VM record exists (cleaned up on `vm rm`); each `vm run` / `vm start` truncates and rewrites it from scratch — `-f` detects the truncation and seeks back to the start of the file so you don't miss the new boot's lines.

| Flag             | Default | Description                                                          |
| ---------------- | ------- | -------------------------------------------------------------------- |
| `--follow`, `-f` | `false` | Stream new log lines as they are written (Ctrl-C to stop)            |
| `--tail`         | `0`     | Show only the last N lines (0 = all); pairs with `-f` for tail+follow |

```
$ cocoon vm logs myvm
cloud-hypervisor:   0.003732s: <vmm> WARN:virtio-devices/src/block.rs:793 -- sparse=on requested but backend does not support sparse operations
$ cocoon vm logs --tail 5 myvm
... last 5 lines ...
$ cocoon vm logs -f --tail 10 myvm
... last 10 lines, then live tail ...
```

### List Flags

Applies to `cocoon vm list`, `cocoon image list`, and `cocoon snapshot list`:

| Flag              | Default  | Description                              |
| ----------------- | -------- | ---------------------------------------- |
| `--format`, `-o`  | `table`  | Output format: `table` or `json`         |

Additionally, `cocoon snapshot list` supports:

| Flag   | Default | Description                              |
| ------ | ------- | ---------------------------------------- |
| `--vm` |         | Only show snapshots belonging to this VM |

## Networking

Cocoon uses [CNI](https://www.cni.dev/) for VM networking. Each NIC is backed by a TAP device wired to the CNI veth via TC ingress redirect — no bridge sits in the data path.

### Architecture

```
Guest virtio-net  ←→  TAP (multi-queue)  ←TC redirect→  veth  ←→  CNI bridge/overlay
```

- **Multi-queue**: each TAP device is created with one queue pair per boot vCPU (`num_queues = 2 × vCPU` in Cloud Hypervisor), enabling per-CPU TX/RX rings for better throughput. Ring depth per queue is configurable via `--queue-size` (default 512; larger values improve bulk download throughput, smaller values improve RPC latency)
- **Offload**: TSO, UFO, and checksum offload are enabled on the virtio-net device; TAP uses `VNET_HDR` for zero-copy GSO passthrough
- **MAC passthrough**: the guest NIC inherits the CNI veth's MAC address, satisfying anti-spoofing requirements of Cilium, Calico eBPF, and VPC ENI plugins
- **MTU sync**: TAP MTU is automatically synced to the veth to prevent silent large-packet drops in overlay or jumbo-frame setups

### Options

- **Default**: 1 NIC with automatic IP assignment via CNI
- **No network**: `--nics 0` creates a VM with no network interfaces
- **Multi-NIC**: `--nics N` creates N interfaces; for cloudimg VMs all NICs are auto-configured via Netplan, for OCI images all NICs are auto-configured via kernel `ip=` parameters
- **Multi-network**: `--network <name>` selects a specific CNI conflist by name (e.g., `--network macvlan`); omitting uses the first conflist alphabetically. The network name is stored in the VM record for recovery after host reboot. Clone allows `--network` override; restore reuses the existing network.
- **Bridge mode**: `--bridge <device>` creates TAP devices directly on an existing Linux bridge (e.g., `--bridge cni0`), bypassing CNI and TC redirect. VMs get IP via DHCP from the bridge. Mutually exclusive with `--network`
- **DNS**: Use `--dns` to set custom DNS servers (comma separated)

### CNI Configuration

All `.conflist` files in `--cni-conf-dir` (default `/etc/cni/net.d`) are loaded at startup. Use `--network <name>` to select one by its `name` field; omitting defaults to the first file alphabetically. A typical bridge config:

```json
{
  "cniVersion": "1.0.0",
  "name": "cocoon",
  "type": "bridge",
  "bridge": "cni0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.22.0.0/16",
    "routes": [{ "dst": "0.0.0.0/0" }]
  }
}
```

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

## Runtime Device Attach (Cloud Hypervisor only)

Cocoon can hot-plug three classes of external resources onto a running VM:

- **Vhost-user-fs** — a file share served by an external `virtiofsd` (or compatible backend) over a Unix socket. Attach surfaces a virtio-fs device in the guest, accessible via `mount -t virtiofs <tag> /mnt/...`.
- **VFIO PCI passthrough** — a host PCI device bound to `vfio-pci` (GPU, NIC, NVMe). Attach hands the device to the guest with IOMMU isolation.
- **Data disks** — an existing raw disk file, surfaced as `/dev/disk/by-id/virtio-<name>`. Cocoon never creates or deletes the backing file: it can outlive any VM and be re-attached elsewhere (a persistent volume).

All three attaches are **runtime-only**: the device lives only for the current VM process and is gone after stop/restart — re-attach after the next start. Cloud Hypervisor itself rejects snapshotting while a vhost-user or VFIO device is attached; cocoon likewise refuses `snapshot save` and `vm hibernate` while an external disk is attached — umount it in-guest, detach, capture, then re-attach after the wake/restore. External volumes are host state cocoon does not own: it never creates, copies, or deletes the backing file, and snapshot, restore, and clone carry no trace of them. All mutating verbs on one VM (attach/detach, net resize, snapshot, hibernate, restore, stop) serialize on a per-VM lock, so concurrent invocations cannot interleave with a capture window.

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

## NIC Hot-Resize (Cloud Hypervisor only)

`cocoon vm net --nics N VM` brings the running VM's NIC count to `N`. To add NICs, cocoon allocates new host TAP/CNI/bridge plumbing and hot-plugs a fresh NIC into the guest. To remove NICs, it pops from the tail (LIFO) via `vm.remove-device` and tears down the host plumbing.

```bash
# Add a second NIC (or any number).
cocoon vm net my-vm --nics 2

# Remove a NIC (LIFO from the tail).
cocoon vm net my-vm --nics 1
```

On NIC removal, cocoon waits for the guest to ACK B0EJ (CH polls `device_tree` until the device disappears) before tearing down the host TAP / veth / CNI lease. If the guest never ACKs within the eject timeout, the command fails and leaves the cocoon record + host plumbing intact so the operator can quiesce the guest (driver unbind, NetworkManager removal, Windows NDIS halt) and retry.

Resize from zero is supported: under CNI, `--nics 0` still provisions a per-VM netns at boot (CH lives in it from the start), so a later `cocoon vm net --nics N` hot-plugs into the same namespace. Bridge mode keeps CH in the host netns regardless of NIC count, so 0→N adds TAPs onto the configured bridge.

## Windows Support

Cocoon supports Windows guests via the `--windows` flag:

```bash
cocoon vm run --windows --name win11 --cpu 2 --memory 4G --storage 15G <cloudimg-url>
```

The `--windows` flag:
- Forces UEFI firmware boot (cloudimg path)
- Enables Hyper-V enlightenments (`kvm_hyperv=on`)
- Skips cloud-init cidata disk generation (Windows does not use cloud-init)

### Requirements

- Cloud Hypervisor **v51+** with our [CH fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) (includes DISCARD fix, virtio-net ctrl_queue fix, and upstream patches — see [KNOWN_ISSUES.md](KNOWN_ISSUES.md))
- UEFI firmware from our [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) (includes ResetSystem fix for ACPI power-button — see [KNOWN_ISSUES.md](KNOWN_ISSUES.md))
- virtio-win **0.1.285** drivers pre-installed in the image (0.1.240 also works for any version of Cloud Hypervisor; newer versions are supported with our CH fork)

### Image

Pre-built images and build automation are maintained in [cocoonstack/windows](https://github.com/cocoonstack/windows).

The Windows image is published as an **OCI artifact** (split qcow2 parts pushed via ORAS), not a runnable OCI container image — use `oras pull` (not `cocoon image pull` or `docker pull`).

```bash
# 1. Pull split parts via oras (https://oras.land)
oras pull ghcr.io/cocoonstack/windows/win11:25h2

# 2. Reassemble and verify
cat windows-11-25h2.qcow2.*.qcow2.part > windows-11-25h2.qcow2
sha256sum -c SHA256SUMS

# 3. Import into Cocoon
cocoon image import win11-25h2 windows-11-25h2.qcow2
```


### Post-Clone Networking

- **DHCP networks**: no action needed, Windows DHCP client auto-configures
- **Static IP**: configure via SAC serial console (`cocoon vm console`)

For more details, see the [Cloud Hypervisor Windows documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/windows.md).

## Firecracker Backend

Cocoon supports [Firecracker](https://github.com/firecracker-microvm/firecracker) as an alternative hypervisor for workloads that prioritize boot speed and resource density.

```bash
# Run with Firecracker (--fc only needed for create/run/debug)
cocoon vm run --fc --name fast-vm ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Other commands auto-detect the backend — no --fc needed
cocoon vm list              # shows both CH and FC VMs
cocoon vm console fast-vm
cocoon vm stop fast-vm

# Clone infers backend from the snapshot
cocoon snapshot save fast-vm --name my-snap
cocoon vm clone my-snap --name clone-vm
```

### Feature Comparison

| Feature | Cloud Hypervisor | Firecracker |
|---------|:---:|:---:|
| OCI images (direct boot) | Y | Y |
| Cloud images (UEFI boot) | Y | N |
| Windows guests | Y | N |
| Snapshot / Clone / Restore | Y | Y |
| Multi-queue networking | Y | N |
| Memory balloon | Y | Y |
| qcow2 storage | Y | N |
| Interactive console | Y | Y |
| HugePages | Y | Y |
| Boot time | ~200-500ms | ~125ms |
| Memory overhead | ~10-20 MiB/VM | <5 MiB/VM |

### Limitations

- **OCI images only**: `--fc` is mutually exclusive with `--windows` and rejects cloudimg (UEFI boot) images
- **Raw disks only**: Firecracker uses raw virtio-blk without serial support; disks are referenced by device path (`/dev/vdX`)
- **Single-queue networking**: `NetworkConfig.NumQueues` is ignored
- **Snapshot portability requires same directory layout**: FC snapshots store absolute paths in the vmstate binary (not patchable); cross-host export/import requires the target host to use the same `root_dir`/`run_dir` and have the same OCI image pulled
- **Console via PTY relay**: a background relay process bridges FC's serial (stdin/stdout) to `console.sock`

### OCI Image Compatibility

OCI images must include a `resolve_disk()` init script that supports device paths (e.g., `/dev/vda`) in addition to virtio serial names. Images built from `os-image/ubuntu/overlay.sh` (v0.3+) support both formats automatically.

## VM Lifecycle

| State      | Description                                              |
| ---------- | -------------------------------------------------------- |
| `creating` | DB placeholder written, disks being prepared             |
| `created`  | Registered, hypervisor process not yet started           |
| `running`  | Hypervisor process alive, guest is up                    |
| `stopped`  | Hypervisor process exited cleanly                        |
| `error`    | Start or stop failed                                     |

### Shutdown Behavior

- **UEFI VMs (cloudimg)**: ACPI power-button → poll for graceful exit → timeout (default 30s, configurable via `stop_timeout_seconds` in config or `--timeout` flag) → SIGTERM → 5s → SIGKILL
- **Windows VMs**: ACPI power-button works with our [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) (~8-13s shutdown once fully booted). The guest ACPI handler needs ~60s from cold boot to initialize; stopping before that triggers the 30s timeout fallback. Clone-restored VMs inherit the ready ACPI state and shut down immediately. With upstream firmware, use `ssh shutdown /s /t 0` before stopping, or `--force` to skip the ACPI timeout (see [KNOWN_ISSUES.md](KNOWN_ISSUES.md))
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

## Snapshot & Clone

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

Snapshots can be exported to portable `.tar.gz` archives for transfer between hosts or clusters, and imported back:

```bash
# Export a snapshot to a file
cocoon snapshot export my-snap -o my-snap.tar.gz

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

- **VM must be running or stopped.** Restore cold-spawns a fresh hypervisor process from the snapshot; a stopped target (e.g. hibernated) gets the `vm start` network self-heal first.
- **Snapshot must belong to the VM.** Only snapshots created from the same VM (tracked in `snapshot_ids`) are accepted; pass `--force` with `--from-dir` to opt into a foreign lineage.
- **CPU, memory, and storage come from the snapshot.** The hypervisor reconstructs the guest from snapshot state, so these are not configurable at restore time; cocoon realigns the persisted record to match.
- **NIC count must match the target VM.** Restore reuses the VM's existing network namespace, TAP devices, and IP allocation; a mismatched count is rejected.

## Status Monitoring

`cocoon vm status` provides real-time VM state monitoring with two modes:

```bash
# Refresh mode (default) — clears and redraws like `watch`
cocoon vm status

# Event stream mode — appends state changes (for scripting / vk-cocoon)
cocoon vm status --event

# Filter specific VMs, custom poll interval
cocoon vm status --event -n 2 my-vm other-vm
```

State changes are detected via **fsnotify** on the VM index file (sub-second latency), with a configurable poll interval as fallback. Event mode emits `ADDED`, `MODIFIED`, and `REMOVED` lines suitable for machine consumption.

## Garbage Collection

`cocoon gc` performs cross-module garbage collection:

1. **Lock** all modules (images, VMs, network, snapshots) — if any module is busy, the entire GC cycle is skipped to maintain consistency
2. **Snapshot** all module indexes under lock
3. **Resolve** each module identifies unreferenced resources using the full snapshot set (e.g., image GC checks VM and snapshot records for blob references)
4. **Collect** — delete identified targets

This ensures blobs referenced by running VMs or saved snapshots are never deleted.

### Log Output

Every collected item is logged at INFO level with a structured `key=value` payload under `gc.<module>`, and a summary line ends the cycle. Sample:

```
INFO gc.snapshot          collected id=XEOU... name=ubuntu-hot-testing:v1 bytes=3221225472 last_accessed=2026-04-12T10:30:00Z reason=lru-age
INFO gc.snapshot          collected id=2GQVEA... name= bytes=0 last_accessed=never reason=orphan
INFO gc.cloudhypervisor   collected id=ABC123 runDir=/var/lib/cocoon/run/cloudhypervisor/ABC123 logDir=/var/log/cocoon/cloudhypervisor/ABC123 reason=orphan-runDir
INFO gc.oci               collected blob=b40150c1c2717d... reason=unreferenced
INFO gc.cni               collected id=JKLMN netns=cocoon-JKLMN nics=2 reason=orphan
INFO gc.bridge            collected id=MNOPQ iface=btMNOPQ-0 reason=orphan-tap
INFO gc.Run               completed: cloudhypervisor=1 cni=1 oci=4 snapshot=3 (failures: 0, duration: 230ms)
```

Filter with `awk` / `grep`:

```bash
journalctl -u cocoon-gc.service --since today | grep "gc.snapshot.*reason=lru-"
journalctl -u cocoon-gc.service --since today | awk '/gc.Run completed/'
```

Reasons:
- **snapshot**: `orphan` (dataDir without DB record), `stale-pending` (Create crashed >24h ago), `lru-all` / `lru-age` / `lru-keep` / `lru-size` (multi-criterion uses `+` joiner)
- **cloudhypervisor / firecracker**: `orphan-runDir`, `orphan-logDir`, `stale-creating`
- **images (oci, cloudimg)**: `unreferenced`
- **cni**: `orphan` (netns without active VM)
- **bridge**: `orphan-tap`

### Snapshot LRU Eviction

Bare `cocoon gc` only reclaims **orphans** (on-disk data with no DB record) and **stale pending** records (crashed mid-Create, older than 24h). To also evict healthy snapshots by access recency, pass `--snapshot`:

| Flag                 | Effect                                                                                          |
| -------------------- | ----------------------------------------------------------------------------------------------- |
| `--snapshot`           | Enable LRU eviction. Bare flag = evict **every** non-pending snapshot.                          |
| `--snapshot-keep N`    | Keep at most N most-recently-accessed snapshots.                                                |
| `--snapshot-age DUR`   | Evict snapshots last accessed before this duration (e.g. `720h` for 30d).                       |
| `--snapshot-size SZ`   | Evict oldest snapshots until total size ≤ this (e.g. `100GB`).                                  |
| `--snapshot-dry-run`   | Log which snapshots would be LRU-evicted; act on nothing. **Snapshot-only — orphans and other GC modules still execute.** |

Sub-flags combine as union of evictions (intersection of kept) — a snapshot is kept only if it passes **every** active criterion. All sub-flags require `--snapshot`; negative values are rejected.

`LastAccessedAt` is updated on `Restore`, `vm clone` (via `DataDir`), `snapshot export`, and `snapshot import` (set to creation time). `Inspect` and `list` do not count as access.

```bash
# Preview what 30-day eviction would remove (snapshot-only — other GC modules still run)
cocoon gc --snapshot --snapshot-age=720h --snapshot-dry-run

# Production: weekly cleanup, keep 50 newest within 7 days
cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50

# Cap storage at 100GB
cocoon gc --snapshot --snapshot-size=100GB

# Nuke all snapshots (dev / test reset)
cocoon gc --snapshot
```

### Scheduled Snapshot GC

`cocoon gc` is a one-shot, lock-safe operation — drive periodic execution from a systemd timer or cron. Recommended template (systemd):

```ini
# /etc/systemd/system/cocoon-gc.service
[Unit]
Description=Cocoon snapshot GC (LRU eviction)

[Service]
Type=oneshot
ExecStart=/usr/local/bin/cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50
StandardOutput=journal
StandardError=journal
```

```ini
# /etc/systemd/system/cocoon-gc.timer
[Unit]
Description=Run cocoon snapshot GC daily

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
Unit=cocoon-gc.service

[Install]
WantedBy=timers.target
```

Enable: `systemctl enable --now cocoon-gc.timer`.

For cron, drop a one-liner into `/etc/cron.daily/cocoon-gc`:

```sh
#!/bin/sh
exec /usr/local/bin/cocoon gc --snapshot --snapshot-age=168h --snapshot-keep=50
```

## OS Images

Pre-built OCI VM images (Ubuntu 22.04, 24.04) are published to GHCR and auto-built by GitHub Actions when `os-image/` changes:

```bash
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:24.04
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:22.04
```

These images include kernel, initramfs, and a systemd-based rootfs with an overlayfs boot script. Every official OS image (Ubuntu + Android) bakes `cocoon-agent` (vsock exec) with auto-start; Ubuntu images additionally enable `sshd` with `PermitRootLogin yes` so `ssh root@<vm>` works out of the box (default `root:cocoon`).

## Shell Completion

```bash
# Bash
cocoon completion bash > /etc/bash_completion.d/cocoon

# Zsh
cocoon completion zsh > "${fpath[1]}/_cocoon"

# Fish
cocoon completion fish > ~/.config/fish/completions/cocoon.fish
```

## Development

```bash
make build    # Build cocoon binary (CGO_ENABLED=0)
make test     # Run tests with race detector and coverage
make lint     # Run golangci-lint
make fmt      # Format code with gofumpt + goimports
make all      # Full pipeline: deps + fmt + lint + test + build
```

See `make help` for all available targets.

## Known Issues

See [`KNOWN_ISSUES.md`](KNOWN_ISSUES.md) for known limitations, workarounds, and CNI configuration guidance.

## License

This project is licensed under the MIT License. See [`LICENSE`](./LICENSE).
