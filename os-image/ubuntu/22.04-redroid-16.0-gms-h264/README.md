# ReDroid 16 GMS VM image

This directory builds a self-contained Cocoon VM rootfs with Ubuntu 22.04
(Jammy), Docker, ReDroid 16, GMS/GApps, ADB, and one RFB/VNC endpoint.

The guest exposes:

- `5555/tcp`: Android ADB.
- `5900/tcp`: one multi-client RFB endpoint. Clients supporting TigerVNC's
  H.264 encoding 50 receive scrcpy's original H.264 stream. Other clients fall
  back to a lazily decoded Tight/JPEG, ZRLE, Hextile, or Raw framebuffer on the
  same port.

There is no X11 framebuffer capture, VNC-to-X11 hop, noVNC, or WebSocket layer.
The Android display is fixed at 720x1280, 320 dpi, 60 fps, with an 8 Mbit/s
scrcpy H.264 stream.

## Boot and persistence

The build creates the ReDroid container and starts only its one-shot bake
wrapper inside a privileged Docker-in-Docker daemon. It then bakes the whole
vfs `/var/lib/docker` (the single-layer image plus a restart-primed
`--restart unless-stopped` container) into the VM rootfs. On boot the VM's
dockerd replays the container from its restart policy —
there is **no first-boot `docker create` and no launcher service**, so the
multi-GB vfs create-copy is off the run/start path and the container is up as
soon as dockerd starts. The VM keeps binder and ashmem in `modules-load.d`, but
its exported rootfs can run under container-detected virtualization where that
loader is skipped. Explicit `docker.service` `ExecStartPre` hooks therefore
load both modules before dockerd replays ReDroid.

Because the container is baked into the immutable EROFS, its vfs rootfs copy is
shared read-only across all VMs/clones of the image; the per-VM COW holds only
the container's runtime writes. Android's `/data` is a fresh per-VM bind volume
(`/var/lib/redroid-data`), so Android itself cold-boots on the first start of
each VM even though the container is already running. Later VM stop/start cycles
reuse the same container and `/data`.

The baked container defaults Android DNS to `8.8.8.8` and `1.1.1.1`. Override
these at image-build time when the deployment needs internal resolvers:

```bash
REDROID_DNS1=<primary-dns> REDROID_DNS2=<secondary-dns> bash build.sh
```

The values are part of the baked container command; they are not runtime
environment variables of an already-published VM image.

Bridge mode is intentional. ReDroid's Android `netd` must not share the VM host
network namespace, where its policy routing can make the VM itself unreachable.
Docker uses the classic `vfs` graphdriver because the Cocoon rootfs already uses
an EROFS-backed overlay and native `overlay2` (overlay-as-upperdir) is kernel
refused. `fuse-overlayfs` is not used: an A/B restart test with the same ReDroid
image showed that its persisted container root became unbootable on the second
start while `vfs` restarted correctly.

## Android image contents

The Android 16 image enables Google Play services, Google Services Framework,
Play Store, Chrome, Trichrome Library, Android System WebView, and Fossify Home.
Fossify Home is the default launcher; Launcher3 remains installed as a recovery
HOME implementation.

The Camera2 application and Chromium WebView test shell (`Browser2`) are removed
physically. Android System WebView is retained because Chrome and other apps
still require a WebView provider. Meituan and Damai are not included.

## VNC clients

The default VNC password is `redroid`.

For the H.264-enabled TigerVNC client, disable automatic encoding selection or
it will override the explicit H.264 choice. Also disable remote resize: changing
the RFB desktop size would require restarting/rescaling the fixed Android scrcpy
session and would invalidate input coordinates.

```bash
vncviewer \
  -AutoSelect=0 \
  -PreferredEncoding=H.264 \
  -RemoteResize=0 \
  -Shared=1 \
  <vm-ip>::5900
```

An ordinary TigerVNC client uses the automatic Tight/JPEG fallback with its
default encoding and resize settings. The server suppresses dynamic
desktop-size extensions for compatibility. If an H.264-capable viewer falls
back to Tight, the server uses lossless Tight to avoid JPEG ABI problems in
specialized client builds; regular VNC clients retain the faster JPEG path:

```bash
vncviewer -Shared=1 <vm-ip>::5900
```

## Build

Each architecture uses its matching native GitHub runner. The DinD bake starts
only a one-shot wrapper for long enough to arm Docker's restart policy; it does
not boot Android on the build host, so CI does not need binder or ashmem. The
wrapper removes its marker from the created container's VFS root. When the VM's
dockerd replays that same container, it immediately execs Android `/init`.

amd64 (x86_64 GMS + libndk ARM translation):

```bash
# 1. prepare gapps16-x86_64.tar from Google's pinned API-36 r07 Play Store image
ARCH=amd64 bash prepare-gapps16.sh
# 2. build the ReDroid 16 GMS image
docker buildx build --platform linux/amd64 --load \
  -f redroid-gms16.Dockerfile -t local/redroid-gms:16.0-cocoon .
# 3. prime the baked container's restart policy and build the VM image
ARCH=amd64 REDROID_SRC=local/redroid-gms:16.0-cocoon \
  VM_TAG=ubuntu-redroid-16.0-gms-h264:22.04-android16 PUSH=0 bash build.sh
```

arm64 (native arm64 GMS, no translator):

```bash
ARCH=arm64 bash prepare-gapps16.sh
docker buildx build --platform linux/arm64 --load \
  --build-arg GAPPS_TAR=gapps16-arm64-v8a.tar \
  -f redroid-gms16.Dockerfile -t local/redroid-gms:16.0-cocoon-arm64 .
ARCH=arm64 REDROID_SRC=local/redroid-gms:16.0-cocoon-arm64 \
  VM_TAG=ubuntu-redroid-16.0-gms-h264:22.04-android16-arm64 PUSH=0 bash build.sh
```

`build.sh` resolves the current `dev` release from `cocoonstack/libvncserver`,
pins its 40-character source commit, verifies the release SHA, verifies that
ReDroid is a single physical layer, bakes the restart-primed container into a
vfs Docker store, and builds the `linux/$ARCH` VM image.

Export a Cocoon-importable rootfs (per arch):

```bash
cid="$(docker create local/ubuntu-redroid-16.0-gms-h264:22.04-android16)"
docker export "$cid" | gzip -9 > redroid16-gms-cocoon-amd64-rootfs.tar.gz
docker rm "$cid"
```

The rootfs export flattens the outer Dockerfile layers. Cocoon converts the tar
stream to an `lz4hc`-compressed EROFS layer during import; the gzip level only
affects transfer size. Then on the target host:

```bash
cocoon image import redroid16-gms:16.0-final \
  redroid16-gms-cocoon-amd64-rootfs.tar.gz
```

## Architecture boundary

Both amd64 and arm64 are supported and built separately into single-arch images.
amd64 bakes an x86_64 ReDroid plus `libndk_translation` so ARM-only apps run
under x86. arm64 bakes a native arm64 ReDroid with no translator: ARM apps run
natively, and the libndk download and `ro.dalvik.vm.native.bridge` build.prop
edits are skipped. ReDroid (a multi-arch image index), the GApps overlay
(`gapps16-<arch>.tar`), and the guest kernel's binder/ashmem modules must all
match the arch. `scrcpy-rfb` and `scrcpy-server` are released for both arches.

## Verification

`verify-vm-inner.sh` runs during the VM Docker build and again against each
published per-architecture image in CI. Runtime checks should additionally
confirm `sys.boot_completed=1`, ports 5555/5900, the baked container coming up on
boot with no launcher service, Docker restart policy and data mount, the enabled
package set above, and concurrent H.264 plus ordinary RFB clients.
