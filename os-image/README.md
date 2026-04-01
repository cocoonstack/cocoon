# Cocoon OS Images

Pre-built OS images are hosted on [GitHub Container Registry](https://github.com/orgs/cocoonstack/packages?repo_name=cocoon).

## Available Images

### Ubuntu

Multi-arch (`linux/amd64`, `linux/arm64`).

| Image | Tag | IMAGE_NAME |
|-------|-----|------------|
| Ubuntu 22.04 (Jammy) | `22.04` | `ghcr.io/cocoonstack/cocoon/ubuntu:22.04` |
| Ubuntu 24.04 (Noble) | `24.04` | `ghcr.io/cocoonstack/cocoon/ubuntu:24.04` |
| Ubuntu 24.04 + Chrome | `24.04-chrome` | `ghcr.io/cocoonstack/cocoon/ubuntu:24.04-chrome` |
| Ubuntu 24.04 + Xfce | `24.04-xface` | `ghcr.io/cocoonstack/cocoon/ubuntu:24.04-xface` |
| Ubuntu 24.04 + PicoClaw | `24.04-picoclaw` | `ghcr.io/cocoonstack/cocoon/ubuntu:24.04-picoclaw` |

### Android (Redroid)

`linux/amd64` only. Runs Android via [Redroid](https://github.com/remote-android/redroid-doc) directly as PID 1 in the VM — no Ubuntu/systemd layer.

| Image | Tag | IMAGE_NAME |
|-------|-----|------------|
| Android 14 | `14.0` | `ghcr.io/cocoonstack/cocoon/android:14.0` |

Access via `adb connect <vm-ip>:5555` or `scrcpy -s <vm-ip>:5555 --no-audio`.

## Quick Start

### Ubuntu

```bash
IMAGE_NAME="ghcr.io/cocoonstack/cocoon/ubuntu:24.04" bash start.sh
```

### Android

```bash
IMAGE_NAME="ghcr.io/cocoonstack/cocoon/android:14.0" bash start.sh
```

## Prerequisites

- Linux with KVM access (`/dev/kvm` must be writable)
- `wget`, `mkfs.erofs`, `mkfs.ext4` installed
- `sudo` required on first run to set `CAP_NET_ADMIN`

## What start.sh Does

1. Downloads [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) and sets capabilities
2. Pulls the container image specified by `IMAGE_NAME` in a daemonless manner via [crane](https://github.com/google/go-containerregistry)
3. Extracts the kernel (`vmlinuz`) and initramfs (`initrd.img`) from the image, and compresses the rootfs into EROFS
4. Creates a 10G COW (Copy-on-Write) disk as the writable layer
5. Launches a Cloud Hypervisor MicroVM (rootless, no daemon required)

## Browse All Available Images

Visit the GitHub Packages page for the full list of images and tags:

https://github.com/orgs/cocoonstack/packages?repo_name=cocoon
