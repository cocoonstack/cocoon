# Images

Cocoon boots two image families: OCI VM images (kernel + rootfs layers, direct boot) and cloud images (qcow2, UEFI boot).

## Pulling

```bash
# OCI VM image from a registry
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:24.04

# Cloud image from an HTTP(S) URL (auto-converted to qcow2 v3)
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# --force re-downloads a URL whose content was replaced upstream (OCI tags are re-resolved on every pull)
cocoon image pull --force https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img
```

Blobs are content-addressed (SHA-256) and deduplicated; OCI layers are converted to EROFS, cloud images to qcow2 v3.

## Importing

`cocoon image import NAME [FILE...]` imports local files or stdin, auto-detected by magic bytes (gzip-wrapped input supported):

- qcow2 (`QFI` magic) → stored as a cloud image
- tar → converted to an EROFS layer in an OCI image
- multiple FILE arguments → split qcow2 parts or multiple tar layers
- no FILE → read from stdin

```bash
cocoon image import myimg disk.qcow2
cat layers.tar.gz | cocoon image import mylayers
```

## Managing

```bash
cocoon image list
cocoon image inspect ubuntu:24.04
cocoon image rm sha256:abc123
```

A digest prefix must be unambiguous: `image rm` errors on a short prefix that matches more than one image (use a longer prefix), and `image inspect` treats an ambiguous prefix as not-found.

## OS Images

Pre-built OCI VM images are published to GHCR and auto-built by GitHub Actions when `os-image/` changes. Three families ship: Ubuntu (22.04, 24.04, plus the `24.04-chrome` / `24.04-xface` / `24.04-picoclaw` variants), Debian (13), and Android (14.0, 15.0, `15.0-gms`, `16.0-gms-h264`). [OS Images](os-image.md) lists every tag.

```bash
cocoon image pull ghcr.io/cocoonstack/cocoon/ubuntu:24.04
cocoon image pull ghcr.io/cocoonstack/cocoon/debian:13
cocoon image pull ghcr.io/cocoonstack/cocoon/android:15.0
```

These images include kernel, initramfs, and a systemd-based rootfs with an overlayfs boot script. Every official OS image (Ubuntu, Debian, Android) bakes `cocoon-agent` (vsock exec) with auto-start; the Ubuntu and Debian images additionally enable `sshd` with `PermitRootLogin yes` so `ssh root@<vm>` works out of the box (default `root:cocoon`).

Build scripts, image contents, and the local `start.sh` harness are documented in [OS Images](os-image.md).
