# OS Images

Official pre-built OCI VM images (Ubuntu, Android), the Windows artifact, and the `os-image/` build harness.

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
| Android 15 | `15.0` | `ghcr.io/cocoonstack/cocoon/android:15.0` |
| Android 15 + GMS + ARM translation | `15.0-gms` | `ghcr.io/cocoonstack/cocoon/android:15.0-gms` |
| Android 16 + GMS + H.264 ([README](../os-image/ubuntu/22.04-redroid-16.0-gms-h264/README.md)) | `16.0-gms-h264` | `ghcr.io/cocoonstack/cocoon/android:16.0-gms-h264` |

Access via `adb connect <vm-ip>:5555` or `scrcpy -s <vm-ip>:5555 --no-audio`.

The `16.0-gms-h264` image is Ubuntu+Redroid-based (built from `os-image/ubuntu/22.04-redroid-16.0-gms-h264/`), unlike the bare `os-image/android/` family above.

The `15.0-gms` variant adds Google Play services (GmsCore, Play Store, GSF)
and `libndk_translation` so arm64-only APKs run on the x86_64 host. It runs
ordinary arm64 apps (verified with Meituan). It does **not** run apps whose
native layer hooks JNI or self-checks for tampering (WeChat, QQ, Alipay,
banking, most anti-cheat games): the translator's proxied JNIEnv breaks those
assumptions and they crash at startup — an x86 binary-translation limit, not a
config gap. Hardened arm64 apps need an arm64 host running the plain `15.0`
image (no translation). See [`os-image/android/15.0-gms/README.md`](../os-image/android/15.0-gms/README.md).

### Windows

`linux/amd64` only. Build automation and pre-built images are maintained in [cocoonstack/windows](https://github.com/cocoonstack/windows).

Pre-built images are published to GHCR as split qcow2 parts (each part ≤ 1.9 GiB to stay within the GHCR per-layer limit):

```
ghcr.io/cocoonstack/windows/win11:25h2              # moving alias, latest good build
ghcr.io/cocoonstack/windows/win11:25h2-<YYYYMMDD>   # dated immutable tag
```

The Windows image is published as an **OCI artifact** (split qcow2 parts pushed via ORAS), not a runnable OCI container image — use `oras pull` (not `cocoon image pull` or `docker pull`).

Pull and import into Cocoon:

```bash
# 1. Pull split parts via oras (https://oras.land)
oras pull ghcr.io/cocoonstack/windows/win11:25h2

# 2. Reassemble and verify
cat windows-11-25h2.qcow2.*.qcow2.part > windows-11-25h2.qcow2
sha256sum -c SHA256SUMS

# 3. Import into Cocoon
cocoon image import win11-25h2 windows-11-25h2.qcow2
cocoon vm run --windows --name win11 --cpu 4 --memory 4G win11-25h2
```

See [cocoonstack/windows](https://github.com/cocoonstack/windows) for build steps and version requirements.

## Quick Start

### Ubuntu

```bash
IMAGE_NAME="ghcr.io/cocoonstack/cocoon/ubuntu:24.04" bash start.sh   # run from os-image/ in the repo
```

### Android

```bash
IMAGE_NAME="ghcr.io/cocoonstack/cocoon/android:14.0" bash start.sh   # run from os-image/ in the repo
```

## In-VM Services

Every official OS image bakes the following on top of its base distro:

- **cocoon-agent** (vsock exec) — pinned binary from [cocoonstack/cocoon-agent](https://github.com/cocoonstack/cocoon-agent), auto-started on boot. Backs `cocoon vm exec` (kubectl-style stdin/stdout/stderr/exit, no SSH/network dependency). Ubuntu uses a systemd unit; Android uses `/system/etc/init/cocoon-agent.rc`.
- **sshd** *(Ubuntu only)* — `openssh-server` enabled with `PermitRootLogin yes`. Default credentials are `root:cocoon`. SSH covers the human-on-keyboard case while cocoon-agent handles control-plane traffic.

Default credentials apply to fresh VMs. If you fork an image you should rotate the root password and (if you keep sshd) flip `PermitRootLogin` back to `no` once you have a non-root sudoer.

## DHCP and VM Cloning

All Ubuntu images configure systemd-networkd with `ClientIdentifier=mac` in their DHCP settings. This ensures that when a VM is cloned from a snapshot, each clone uses its unique MAC address as the DHCP client identifier instead of the machine-id-derived DUID. Without this, clones from the same snapshot share an identical DUID and dnsmasq treats them as a single client, causing IP conflicts.

The setting is applied in two places:
- `os-image/ubuntu/network.sh` — the initramfs DHCP fallback path
- Each Dockerfile's `20-wired.network` — the default systemd-networkd config

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
