# Cocoon OS Images

Image catalog, in-VM services, and build documentation moved to
[docs/os-image.md](../docs/os-image.md)
(rendered at [cocoonstack.github.io/cocoon/os-image](https://cocoonstack.github.io/cocoon/os-image)).

## Quick Start

```bash
IMAGE_NAME="ghcr.io/cocoonstack/cocoon/ubuntu:24.04" bash start.sh
```

`start.sh` pulls the image daemonlessly, extracts kernel/initramfs, builds
the EROFS rootfs and a COW disk, and boots a rootless Cloud Hypervisor
MicroVM.
