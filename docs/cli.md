# CLI Reference

Every command and flag. Concept guides: [VM lifecycle](vm.md), [networking](networking.md), [snapshots](snapshots.md), [devices](devices.md), [GC](gc.md).

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
| `--data-disk` | empty (repeatable) | Attach an extra data disk: `size=20G[,name=...][,fstype=ext4|none][,mount=/mnt/x][,directio=on|off|auto]`. See [Data Disks](vm.md#data-disks) |
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
| `--restore-mode` | `copy`           | Memory restore mode: `copy`, `ondemand` (UFFD) or `mmap` (CoW map, shares page cache across clones); CH only, non-copy modes require the snapshot file to remain on disk |
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
| `--restore-mode` | `copy` | Memory restore mode: `copy`, `ondemand` (UFFD) or `mmap` (CoW map); CH only, non-copy modes require the snapshot file to remain on disk |
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
| `--output`, `-o` |  `<name-or-id>.tar` (`.tar.gz` with `--gzip`) | Output file path (`-` for stdout)                 |
| `--gzip`        | `false`                    | Compress output with gzip                         |
| `--to-dir`      |                            | Export into a directory (must be empty/absent) instead of a tar; pairs with `vm clone --from-dir` |

`--to-dir` writes a `snapshot.json` envelope alongside reflink-copied data files. Useful for NFS golden images or rsync-friendly handoff: `cocoon snapshot export snap --to-dir /nfs/golden && rsync ...`. Mutually exclusive with `--output` and `--gzip`.

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
| `--interval`, `-n` | `5`     | Poll interval in seconds (only with `--watch` or `--event`) |
| `--watch`, `-w`    | `false` | Refresh-loop mode (full-screen redraw each tick); omit for a one-shot snapshot |
| `--event`          | `false` | Event stream mode (append changes instead of refreshing) |
| `--format`         |         | Output format: `json` (one-shot and event modes; `--watch` always renders a table) |

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

| Flag                  | Default | Description                                        |
| --------------------- | ------- | -------------------------------------------------- |
| `--env`, `-e`         |         | Extra env var `KEY=VALUE` (repeatable)              |
| `--interactive`, `-i` | `false` | Attach the caller's stdin to the command (otherwise stdin closes immediately) |

```
$ cocoon vm exec myvm -- uname -n
myvm
$ echo hello | cocoon vm exec -i myvm -- cat
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
