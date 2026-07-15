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
the container's runtime writes. vfs otherwise stores the image layer, the
container, and its init layer as three near-identical copies, so `build.sh`
hardlinks the identical files before taring — safe because the EROFS lower is
read-only and overlay copy-up isolates every write. Android's `/data` is a fresh per-VM bind volume
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
Fossify Home is the default launcher; the first-boot hook sets it as HOME early
(before the GMS staging step) so Android does not show the home-picker chooser.
Launcher3/Quickstep stays enabled as a fallback and because it provides the
SystemUI navigation bar and recents — disabling it removes the on-screen buttons.

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

This image is built by the shared `build-os-images.yml` workflow via an optional
`ci-prepare.sh` hook (no dedicated workflow). On one amd64 runner the hook bakes
both arches — amd64 native, arm64 emulated under QEMU — because the DinD bake
only arms a one-shot wrapper that sleeps for long enough to activate Docker's
restart policy; it does not boot Android on the build host, so no binder, ashmem,
or native arm64 runner is needed. The wrapper removes its marker from the created
container's VFS root; when the VM's dockerd replays that container, it execs
Android `/init`. `build-push-action` then builds the multi-arch VM image, each
platform ADDing its own `docker-data-<arch>.tar`, and publishes `android:16.0-gms-h264`
(`publish-as`) — the canonical tag only from master, an `-<sha>` tag on every run.

On a new Android data volume, the boot-complete hook registers the bundled GMS
Core APK once as a Package Manager update under `/data/app`. Google's Play
emulator seeds GMS Core and its install-time Chimera splits in userdata, but the
portable overlay above intentionally carves only `product` and `system_ext`.
Without this one-time step the remaining system container APK reports no usable
Chimera modules. The persisted `/var/lib/redroid-data` bind makes later VM
stop/start cycles a path check only; GMS Core is not reinstalled on every boot.

A local build (amd64 shown; use `arm64`/`arm64-v8a` for arm64) mirrors what
`ci-prepare.sh` and the shared build step do — carve GApps, build the flattened
ReDroid source image, bake `docker-data-<arch>.tar`, then build the VM image from
the ubuntu family dir:

```bash
cd os-image/ubuntu/22.04-redroid-16.0-gms-h264
ARCH=amd64 bash prepare-gapps16.sh                       # -> gapps16-x86_64.tar
docker buildx build --load --platform linux/amd64 \
  --build-arg TARGETARCH=amd64 --build-arg GAPPS_TAR=gapps16-x86_64.tar \
  -f redroid-gms16.Dockerfile -t local/redroid-gms:16.0-cocoon-amd64 .
ARCH=amd64 REDROID_SRC=local/redroid-gms:16.0-cocoon-amd64 bash build.sh   # -> docker-data-amd64.tar
docker buildx build --load --platform linux/amd64 -t local/android:16.0-gms-h264 \
  --secret id=cocoon_overlay,src=../overlay.sh \
  --secret id=cocoon_network,src=../network.sh \
  --secret id=cocoon_install_agent,src=../install-agent.sh \
  --secret id=daemon_json,src=daemon.json \
  -f Dockerfile ..
```

`build.sh` is a bake-only worker: it verifies ReDroid is a single physical layer,
bakes the restart-primed container into a vfs Docker store, and hardlink-dedups +
tars it to `docker-data-<arch>.tar`. The VM Dockerfile resolves the scrcpy-rfb
release itself (no build-arg from the shared step).

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
