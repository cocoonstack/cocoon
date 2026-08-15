# Cocoon-Compatible Debian 13 Image

This directory builds a bootable Debian 13 (`trixie`) OCI image for Cocoon on
`linux/amd64` and `linux/arm64`. Each platform includes its matching Debian
cloud kernel and checksum-verified Cocoon agent, plus the shared initramfs,
EROFS/overlay boot hooks, systemd networking, and SSH configuration.

## Build and Validate Locally

The validator uses Podman directly when it is available and otherwise falls
back to Docker; set `CONTAINER_ENGINE=podman` or `CONTAINER_ENGINE=docker` to
select one explicitly. Building or validating a non-native platform also
requires QEMU user emulation with the corresponding `binfmt` handler
registered; Podman Desktop normally provides this, while Linux Podman hosts
must configure it separately.

Build each platform with its own tag so Podman stores both architecture-specific
images locally. Run from the repository root:

```bash
AMD64_IMAGE=cocoon-debian-13:local-amd64

podman build \
  --platform linux/amd64 \
  --no-cache \
  --file os-image/debian/13/Dockerfile \
  --secret id=cocoon_overlay,src=os-image/debian/overlay.sh \
  --secret id=cocoon_network,src=os-image/debian/network.sh \
  --secret id=cocoon_install_agent,src=os-image/debian/install-agent.sh \
  --tag "$AMD64_IMAGE" \
  os-image/debian

os-image/debian/validate-image.sh "$AMD64_IMAGE"

ARM64_IMAGE=cocoon-debian-13:local-arm64

podman build \
  --platform linux/arm64 \
  --no-cache \
  --file os-image/debian/13/Dockerfile \
  --secret id=cocoon_overlay,src=os-image/debian/overlay.sh \
  --secret id=cocoon_network,src=os-image/debian/network.sh \
  --secret id=cocoon_install_agent,src=os-image/debian/install-agent.sh \
  --tag "$ARM64_IMAGE" \
  os-image/debian

os-image/debian/validate-image.sh "$ARM64_IMAGE"
```
