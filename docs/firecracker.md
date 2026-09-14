# Firecracker Backend

The `--fc` backend: trade features for boot speed and density.

## Overview

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
| HugePages | Y (opt-in `--hugepages`) | N (would break snapshot restore) |
| Device hot-plug (disk, fs, VFIO) and NIC resize | Y | Only with `--pci` |
| Boot time (indicative, not measured here) | ~200-500ms | ~125ms |
| Memory overhead (indicative, not measured here) | ~10-20 MiB/VM | <5 MiB/VM |

### Limitations

- **OCI images only**: `--fc` is mutually exclusive with `--windows`, `--shared-memory`, `--hugepages`, `--mergeable` and `--no-watchdog`, and rejects cloudimg (UEFI boot) images
- **MMIO by default**: without `--pci` (fixed for the VM lifetime, inherited by snapshots) disk attach/detach, NIC resize and clone-time `--data-disk`/`--nics` are refused
- **io_uring required**: writable disks use the `Async` engine with no opt-out, so a restrictive seccomp profile (Docker's default) breaks them; `--no-direct-io` is ignored
- **Stop waits the full timeout**: FC guests without i8042 never answer CtrlAltDel, so `vm stop` waits out `stop_timeout_seconds` before SIGKILL
- **Clone MTU must match**: a clone requires the target network's MTU to equal the snapshot's
- **Raw disks only**: Firecracker uses raw virtio-blk without serial support; disks are referenced by device path (`/dev/vdX`)
- **Single-queue networking**: `NetworkConfig.NumQueues` is ignored
- **Snapshot portability requires same directory layout**: FC snapshots store absolute paths in the vmstate binary; cocoon bind-mounts the snapshot's drives into place inside a private mount namespace and re-anchors them after resume, so a differing `run_dir` fails outright (`unusable source ... is outside the managed run root`) rather than corrupting the clone, and the same OCI image must be pulled
- **Console via PTY relay**: a background relay process bridges FC's serial (stdin/stdout) to `console.sock`

### OCI Image Compatibility

OCI images must include a `resolve_disk()` init script that supports device paths (e.g., `/dev/vda`) in addition to virtio serial names. Every current `os-image/` family supports both forms.
