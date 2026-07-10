# Cocoon

Lightweight MicroVM engine with dual hypervisor backends: [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) (default) and [Firecracker](https://github.com/firecracker-microvm/firecracker).

**Documentation: [cocoonstack.github.io/cocoon](https://cocoonstack.github.io/cocoon/)** (source in [`docs/`](docs/)).

## Highlights

- **OCI VM images** — pull container-registry images with kernel + rootfs layers, content-addressed EROFS blob cache; **cloud images** from HTTP(S) URLs with automatic qcow2 conversion; local file/stdin import
- **Docker-like CLI** — `create`, `run`, `start`, `stop`, `list`, `inspect`, `console`, `exec`, `rm`, `clone`, `status`
- **Snapshot & clone** — capture a running VM (memory, disks, config) and clone it into new VMs with fresh network identity; restore and atomic hibernate in place; portable export/import between hosts
- **CNI networking** — multi-queue virtio-net TAPs wired via TC redirect (no bridge in the data path), multi-NIC, bridge mode, NIC hot-resize
- **Data disks & runtime attach** — extra virtio-blk disks at create/clone time; hot-plug vhost-user-fs shares, VFIO PCI devices, and external raw disks (CH)
- **Windows guests** — UEFI + Hyper-V enlightenments via the cocoonstack CH/firmware forks
- **Firecracker backend** — `--fc` for ~125ms boots and <5 MiB per-VM overhead (OCI images only)
- **Zero-daemon architecture** — one hypervisor process per VM; modular lock-safe GC with snapshot LRU eviction

## Quick Start

```bash
# One-time environment setup (installs CH, firmware, CNI plugins)
curl -fsSL -o cocoon-check https://raw.githubusercontent.com/cocoonstack/cocoon/refs/heads/master/doctor/check.sh
install -m 0755 cocoon-check /usr/local/bin/ && sudo cocoon-check --upgrade

# Pull an image and run a VM
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:24.04
cocoon vm run --name my-vm --cpu 2 --memory 1G ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Interact
cocoon vm console my-vm
cocoon vm exec my-vm -- uname -a

# Snapshot and clone
cocoon snapshot save --name base my-vm
cocoon vm clone base --name fresh

# Clean up
cocoon vm rm --force my-vm fresh
```

Full walkthroughs: [Installation](docs/install.md) · [CLI reference](docs/cli.md) · [Images](docs/images.md) · [VM lifecycle](docs/vm.md) · [Networking](docs/networking.md) · [Snapshots & clone](docs/snapshots.md) · [Device attach](docs/devices.md) · [Windows](docs/windows.md) · [Firecracker](docs/firecracker.md) · [GC](docs/gc.md) · [OS images](docs/os-image.md) · [Known issues](docs/known-issues.md)

## Development

```bash
make build    # Build cocoon binary (CGO_ENABLED=0)
make test     # Run tests with race detector and coverage
make lint     # Run golangci-lint (GOOS=linux + darwin)
make fmt      # Format code with gofumpt + goimports
make all      # Full pipeline: deps + fmt + lint + test + build
```

## License

This project is licensed under the MIT License. See [`LICENSE`](./LICENSE).
