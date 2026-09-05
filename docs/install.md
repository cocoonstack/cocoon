# Installation

Requirements, install paths, the doctor script, and a first VM.

## Requirements

- Linux with KVM (x86_64 or aarch64)
- Root access (sudo)
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v54 or newer. `cocoon-check --upgrade` installs the [cocoonstack fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) `dev` release build (upstream main plus diff snapshots and a QCOW cluster-leak fix), verified against the release checksums; v53.0 and older have no CopyOnWrite memory restore, so the default clone/restore mode (`mmap`) is rejected and `cocoon-check` reports it
- [Firecracker](https://github.com/firecracker-microvm/firecracker) v1.16.1 or newer (optional, for the `--fc` backend). `cocoon-check --upgrade` installs the [cocoonstack fork](https://github.com/cocoonstack/firecracker/tree/dev) `dev` release build (upstream main plus release CI), verified against the release checksums — the build the `--pci` hot-plug and NIC MTU paths are validated on. `vm clone` needs >= v1.16 for the vsock override, and v1.16.0 permanently breaks guest vsock after any restore, so v1.16.1 is the effective floor (see [known issues](known-issues.md))
- `qemu-img` (from qemu-utils, for cloud images)
- `mkfs.erofs` from erofs-utils **>= 1.8** (for OCI images; 1.7.x tar mode
  silently corrupts layers — cocoon refuses to convert with older versions)
- UEFI firmware (`CLOUDHV.fd`, for cloud images, not needed with `--fc`); on x86_64 `cocoon-check --upgrade` installs the [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) `dev` build (EFI ResetSystem for ACPI power-button shutdown and the IA32_FEATURE_CONTROL/VMXON lock, both needed by Windows guests — see [known issues](known-issues.md))
- CNI plugins (`bridge`, `host-local`, `loopback`)
- Go 1.27+ (build only)

## Installation

### GitHub Releases

Download pre-built binaries from [GitHub Releases](https://github.com/cocoonstack/cocoon/releases):

```bash
# Linux amd64
curl -fsSL -o cocoon.tar.gz https://github.com/cocoonstack/cocoon/releases/latest/download/cocoon_Linux_x86_64.tar.gz

# Linux arm64
curl -fsSL -o cocoon.tar.gz https://github.com/cocoonstack/cocoon/releases/latest/download/cocoon_Linux_arm64.tar.gz

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

This produces a `cocoon` binary in the project root.

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
- Cloud Hypervisor from the cocoonstack fork `dev` release (checksum-verified) and upstream ch-remote (static binaries)
- Firecracker from the cocoonstack fork `dev` release (checksum-verified)
- CLOUDHV.fd firmware: the cocoonstack firmware fork `dev` build on x86_64 (checksum-verified), upstream rust-hypervisor-firmware on aarch64
- CNI plugins (bridge, host-local, loopback, etc.)

Release tags and versions are overridable through `CH_REF`, `CH_REMOTE_VERSION`, `FC_REF`, `FW_REF`, `FW_VERSION` and `CNI_VERSION` (see `cocoon-check --help`).

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
