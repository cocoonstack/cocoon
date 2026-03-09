# Cocoon

Lightweight MicroVM engine built on [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor).

## Features

- **OCI VM images** — pull OCI images with kernel + rootfs layers, content-addressed blob cache with SHA-256 deduplication
- **Cloud image support** — pull from HTTP/HTTPS URLs (e.g. Ubuntu cloud images), automatic qcow2 conversion
- **UEFI boot** — CLOUDHV.fd firmware by default; direct kernel boot for OCI images (auto-detected)
- **COW overlays** — copy-on-write disks backed by shared base images (raw for OCI, qcow2 for cloud images)
- **CNI networking** — automatic NIC creation via CNI plugins, multi-NIC support, per-VM IP allocation
- **Multi-queue virtio-net** — TAP devices created with per-vCPU queue pairs; TSO/UFO/csum offload enabled by default
- **TC redirect I/O path** — veth ↔ TAP wired via ingress qdisc + mirred redirect (no bridge in the data path)
- **DNS configuration** — custom DNS servers injected into VMs via kernel cmdline (OCI) or cloud-init network-config (cloudimg)
- **Cloud-init metadata** — automatic NoCloud cidata FAT12 disk for cloudimg VMs (hostname, root password, multi-NIC Netplan v2 network-config); cidata is automatically skipped on subsequent boots
- **Hugepages** — automatic detection of host hugepage configuration; VM memory backed by hugepages when available
- **Memory balloon** — 25% of memory returned via virtio-balloon (deflate-on-OOM, free-page reporting) when memory >= 256 MiB
- **Graceful shutdown** — ACPI power-button for UEFI VMs with configurable timeout, fallback to SIGTERM → SIGKILL
- **Interactive console** — `cocoon vm console` with bidirectional PTY relay, SSH-style escape sequences (`~.` disconnect, `~?` help), configurable escape character, SIGWINCH propagation
- **Snapshot & clone** — `cocoon snapshot save` captures a running VM's full state (memory, disks, config); `cocoon vm clone` restores it as a new VM with fresh network and identity, resource inheritance with validation
- **Docker-like CLI** — `create`, `run`, `start`, `stop`, `list`, `inspect`, `console`, `rm`, `debug`, `clone`
- **Structured logging** — configurable log level (`--log-level`), log rotation (max size / age / backups)
- **Debug command** — `cocoon vm debug` generates a copy-pasteable `cloud-hypervisor` command for manual debugging
- **Zero-daemon architecture** — one Cloud Hypervisor process per VM, no long-running daemon
- **Garbage collection** — modular lock-safe GC with cross-module snapshot resolution; protects blobs referenced by running VMs and snapshots
- **Doctor script** — pre-flight environment check and one-command dependency installation

## Requirements

- Linux with KVM (x86_64 or aarch64)
- Root access (sudo)
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v51.0+
- `qemu-img` (from qemu-utils, for cloud images)
- UEFI firmware (`CLOUDHV.fd`, for cloud images)
- CNI plugins (`bridge`, `host-local`, `loopback`)
- Go 1.25+ (build only)

## Installation

### GitHub Releases

Download pre-built binaries from [GitHub Releases](https://github.com/projecteru2/cocoon/releases):

```bash
# Linux amd64
curl -fsSL -o cocoon https://github.com/projecteru2/cocoon/releases/download/v0.2.0/cocoon_0.2.0_Linux_x86_64.tar.gz
tar -xzf cocoon_0.2.0_Linux_x86_64.tar.gz
install -m 0755 cocoon /usr/local/bin/

# Or use go install
go install github.com/projecteru2/cocoon@latest
```

### Build from source

```bash
git clone https://github.com/projecteru2/cocoon.git
cd cocoon
make build
```

This produces a `cocoon` binary in the project root. Use `make install` to install into `$GOPATH/bin`.

## Doctor

Cocoon ships a diagnostic script that checks your environment and can auto-install all dependencies:

```bash
# Get script
curl -fsSL -o cocoon-check https://raw.githubusercontent.com/projecteru2/cocoon/refs/heads/master/doctor/check.sh
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
- CLOUDHV.fd firmware (rust-hypervisor-firmware)
- CNI plugins (bridge, host-local, loopback, etc.)

## Quick Start

```bash
# Set up the environment (first time)
sudo cocoon-check --upgrade

# Pull an OCI VM image
cocoon image pull ghcr.io/projecteru2/cocoon/ubuntu:24.04

# Or pull a cloud image from URL
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Create and start a VM
cocoon vm run --name my-vm --cpu 2 --memory 1G ghcr.io/projecteru2/cocoon/ubuntu:24.04

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
│   ├── pull IMAGE [IMAGE...]      Pull OCI image(s) or cloud image URL(s)
│   ├── list (alias: ls)           List locally stored images
│   ├── rm ID [ID...]              Delete locally stored image(s)
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
│   ├── rm [flags] VM [VM...]      Delete VM(s) (--force to stop first)
│   ├── restore [flags] VM SNAP   Restore a running VM to a snapshot
│   └── debug [flags] IMAGE        Generate CH launch command (dry run)
├── snapshot
│   ├── save [flags] VM            Create a snapshot from a running VM
│   ├── list (alias: ls)           List all snapshots
│   ├── inspect SNAPSHOT           Show detailed snapshot info (JSON)
│   └── rm SNAPSHOT [SNAPSHOT...]  Delete snapshot(s)
├── gc                             Remove unreferenced blobs and VM dirs
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
| `--root-password` | `COCOON_DEFAULT_ROOT_PASSWORD` |                    | Default root password for cloudimg VMs |
| `--dns`           | `COCOON_DNS`                   | `8.8.8.8,1.1.1.1`  | DNS servers for VMs (comma separated)  |

## VM Flags

Applies to `cocoon vm create`, `cocoon vm run`, and `cocoon vm debug`:

| Flag        | Default          | Description                                   |
| ----------- | ---------------- | --------------------------------------------- |
| `--name`    | `cocoon-<image>` | VM name                                       |
| `--cpu`     | `2`              | Boot CPUs                                     |
| `--memory`  | `1G`             | Memory size (e.g., 512M, 2G)                  |
| `--storage` | `10G`            | COW disk size (e.g., 10G, 20G)                |
| `--nics`    | `1`              | Number of network interfaces (0 = no network) |
| `--network` | empty (default)  | CNI conflist name (empty = first conflist)     |

### Clone Flags

Applies to `cocoon vm clone`:

| Flag        | Default                  | Description                                             |
| ----------- | ------------------------ | ------------------------------------------------------- |
| `--name`    | `cocoon-clone-<id>`      | VM name                                                 |
| `--cpu`     | `0` (inherit)            | Boot CPUs (must be >= snapshot value)                    |
| `--memory`  | empty (inherit)          | Memory size (must be >= snapshot value)                  |
| `--storage` | empty (inherit)          | COW disk size (must be >= snapshot value)                |
| `--nics`    | `0` (inherit)            | Number of NICs (must be >= snapshot value)               |
| `--network` | empty (inherit)          | CNI conflist name (empty = inherit from source VM)       |

### Snapshot Flags

Applies to `cocoon snapshot save`:

| Flag            | Default | Description          |
| --------------- | ------- | -------------------- |
| `--name`        |         | Snapshot name        |
| `--description` |         | Snapshot description |

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

- **Multi-queue**: each TAP device is created with one queue pair per boot vCPU (`num_queues = 2 × vCPU` in Cloud Hypervisor), enabling per-CPU TX/RX rings for better throughput
- **Offload**: TSO, UFO, and checksum offload are enabled on the virtio-net device; TAP uses `VNET_HDR` for zero-copy GSO passthrough
- **MAC passthrough**: the guest NIC inherits the CNI veth's MAC address, satisfying anti-spoofing requirements of Cilium, Calico eBPF, and VPC ENI plugins
- **MTU sync**: TAP MTU is automatically synced to the veth to prevent silent large-packet drops in overlay or jumbo-frame setups

### Options

- **Default**: 1 NIC with automatic IP assignment via CNI
- **No network**: `--nics 0` creates a VM with no network interfaces
- **Multi-NIC**: `--nics N` creates N interfaces; for cloudimg VMs all NICs are auto-configured via Netplan, for OCI images all NICs are auto-configured via kernel `ip=` parameters
- **Multi-network**: `--network <name>` selects a specific CNI conflist by name (e.g., `--network macvlan`); omitting uses the first conflist alphabetically. The network name is stored in the VM record for recovery after host reboot. Clone allows `--network` override; restore reuses the existing network.
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
- **user-data**: `#cloud-config` with optional root password (`--root-password`)
- **network-config**: Netplan v2 format with MAC-matched ethernets, static IP/gateway/DNS per NIC
- **user-data write_files**: fallback `/etc/systemd/network/15-cocoon-id*.network` files matching current MAC (`MACAddress=`), used when netplan PERM-MAC matching cannot apply

The cidata disk is **automatically excluded on subsequent boots** — after the first successful start, the VM record is marked as `first_booted` and the cidata disk is no longer attached, preventing cloud-init from re-running.

## VM Lifecycle

| State      | Description                                              |
| ---------- | -------------------------------------------------------- |
| `creating` | DB placeholder written, disks being prepared             |
| `created`  | Registered, cloud-hypervisor process not yet started     |
| `running`  | Cloud-hypervisor process alive, guest is up              |
| `stopped`  | Cloud-hypervisor process exited cleanly                  |
| `error`    | Start or stop failed                                     |

### Shutdown Behavior

- **UEFI VMs (cloudimg)**: ACPI power-button → poll for graceful exit → timeout (default 30s, configurable via `stop_timeout_seconds` in config) → SIGTERM → 5s → SIGKILL
- **Direct-boot VMs (OCI)**: `vm.shutdown` API → SIGTERM → 5s → SIGKILL (no ACPI support)
- PID ownership is verified before sending signals to prevent killing unrelated processes

## Performance Tuning

- **Hugepages**: automatically detected from `/proc/sys/vm/nr_hugepages`; when available, VM memory is backed by 2 MiB hugepages for reduced TLB pressure
- **Disk I/O**: multi-queue virtio-blk with `num_queues` matching boot CPUs and `queue_size=256`; host page cache enabled (`direct=off`) for EROFS layers and COW raw disks
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

# 4. Clone with more resources
cocoon vm clone --name big-clone --cpu 4 --memory 4G my-snap

# 5. Delete a snapshot
cocoon snapshot rm my-snap
```

### What Gets Captured

A snapshot contains the full VM state:
- **Memory**: complete RAM contents (memory-ranges)
- **Disks**: COW disk (raw or qcow2), cidata disk (cloudimg)
- **Config**: Cloud Hypervisor config.json and device state (state.json)
- **Metadata**: image reference, CPU/memory/storage/NIC count for resource inheritance

### Clone Constraints

**Resources can be increased, not decreased.** Clone validates that CPU, memory, storage, and NIC count are >= the snapshot's original values. Omitting a flag (or passing 0) inherits the snapshot value.

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

### Restore

Restore reverts a **running** VM to a previous snapshot's state in-place:

```bash
# Restore a VM to a previous snapshot
cocoon vm restore my-vm my-snap

# Restore with more resources (must be >= snapshot values)
cocoon vm restore --cpu 4 --memory 4G my-vm my-snap
```

Cocoon internally restarts the Cloud Hypervisor process with the snapshot's memory and disk state. Network is fully preserved — same IP, same MAC, same network namespace. No guest-side reconfiguration is needed (unlike clone).

### Restore Constraints

- **VM must be running.** Restore operates on a live VM by restarting its CH process with snapshot state. For stopped VMs, use `cocoon vm clone` instead.
- **Snapshot must belong to the VM.** Only snapshots created from the same VM (tracked in `snapshot_ids`) are accepted. Cross-VM restore is not supported; use `cocoon vm clone` for that.
- **NIC count must match.** The VM's current NIC count must equal the snapshot's (restore reuses the VM's existing network, unlike clone which creates fresh NICs and hot-swaps).
- **Resources can be increased, not decreased.** CPU, memory, and storage must be >= the snapshot's original values. Omitting a flag keeps the VM's current value.

## Garbage Collection

`cocoon gc` performs cross-module garbage collection:

1. **Lock** all modules (images, VMs, network, snapshots) — if any module is busy, the entire GC cycle is skipped to maintain consistency
2. **Snapshot** all module indexes under lock
3. **Resolve** each module identifies unreferenced resources using the full snapshot set (e.g., image GC checks VM and snapshot records for blob references)
4. **Collect** — delete identified targets

This ensures blobs referenced by running VMs or saved snapshots are never deleted.

## OS Images

Pre-built OCI VM images (Ubuntu 22.04, 24.04) are published to GHCR and auto-built by GitHub Actions when `os-image/` changes:

```bash
cocoon image pull ghcr.io/projecteru2/cocoon/ubuntu:24.04
cocoon image pull ghcr.io/projecteru2/cocoon/ubuntu:22.04
```

These images include kernel, initramfs, and a systemd-based rootfs with an overlayfs boot script.

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
make vet      # Run go vet for linux and darwin
make fmt      # Format code with gofumpt + goimports
make ci       # Full CI pipeline: fmt-check + vet + lint + test + build
```

See `make help` for all available targets.

## Known Issues

See [`KNOWN_ISSUES.md`](KNOWN_ISSUES.md) for known limitations, workarounds, and CNI configuration guidance.

## License

This project is licensed under the MIT License. See [`LICENSE`](./LICENSE).
