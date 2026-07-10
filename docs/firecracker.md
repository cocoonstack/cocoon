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
