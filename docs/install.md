# Installation

Requirements, install paths, the doctor script, and a first VM.

## Requirements

- Linux with KVM (x86_64 or aarch64)
- Root access (sudo)
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v51.0+ (for Windows VMs and `--restore-mode mmap`, use our [CH fork](https://github.com/cocoonstack/cloud-hypervisor/tree/dev) and [firmware fork](https://github.com/cocoonstack/rust-hypervisor-firmware/tree/dev) for full compatibility — see [known issues](known-issues.md))
- [Firecracker](https://github.com/firecracker-microvm/firecracker) v1.16+ (optional, for `--fc` backend; `vm clone` requires >= v1.16 for the vsock override)
- `qemu-img` (from qemu-utils, for cloud images)
- `mkfs.erofs` from erofs-utils **>= 1.8** (for OCI images; 1.7.x tar mode
  silently corrupts layers — cocoon refuses to convert with older versions)
- UEFI firmware (`CLOUDHV.fd`, for cloud images, not needed with `--fc`)
- CNI plugins (`bridge`, `host-local`, `loopback`)
- Go 1.27+ (build only)

## Installation

### GitHub Releases

Download pre-built binaries from [GitHub Releases](https://github.com/cocoonstack/cocoon/releases):

```bash
# Linux amd64
curl -fsSL -o cocoon.tar.gz https://github.com/cocoonstack/cocoon/releases/download/v0.5.9/cocoon_0.5.9_Linux_x86_64.tar.gz

# Linux arm64
curl -fsSL -o cocoon.tar.gz https://github.com/cocoonstack/cocoon/releases/download/v0.5.9/cocoon_0.5.9_Linux_arm64.tar.gz

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
