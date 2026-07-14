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

The build loads the flattened, single-layer ReDroid image into a privileged
Docker-in-Docker daemon. It deliberately does not create a container. The
image-only `/var/lib/docker` is baked into the VM rootfs.

On the first VM boot, `redroid.service` creates and starts the container. With
the `vfs` graphdriver this makes one full rootfs copy in the VM's writable
Cocoon COW layer instead of baking that copy into the immutable EROFS. Later VM
stop/start cycles reuse the same container and `/data`; they do not run another
`docker create` or reinstall Android. The container has:

- `--restart unless-stopped`
- `/var/lib/redroid-data:/data`
- Docker bridge networking with `5555:5555`

Bridge mode is intentional. ReDroid's Android `netd` must not share the VM host
network namespace, where its policy routing can make the VM itself unreachable.
Docker uses the classic `vfs` graphdriver because the Cocoon rootfs already uses
an EROFS-backed overlay and native `overlay2` cannot be nested. `fuse-overlayfs`
is not used: an A/B restart test with the same ReDroid image showed that its
persisted container root became unbootable on the second start while `vfs`
restarted correctly. Requiring the Android image to have one physical layer and
deferring container creation prevents the old build-time VFS copy explosion.

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

For a local test through the offline host:

```bash
ssh -N -L 15900:<vm-ip>:5900 root@10.104.192.182
vncviewer -Shared=1 127.0.0.1::15900
```

## Build

First prepare `gapps16-x86_64.tar` from the pinned Google API-36 Play Store
system image, then build the merged ReDroid image:

```bash
docker buildx build --platform linux/amd64 --load \
  -f redroid-gms16.Dockerfile \
  -t local/redroid-gms:16.0-cocoon .
```

Build the Cocoon VM image from an arm64 or amd64 Docker host:

```bash
REDROID_SRC=local/redroid-gms:16.0-cocoon \
VM_IMAGE=local/ubuntu-redroid-gms-h264:22.04-android16 \
bash build-cross.sh
```

`build-cross.sh` resolves the current `dev` release from
`cocoonstack/libvncserver`, pins its 40-character source commit, verifies the
release SHA, verifies that ReDroid is a single physical layer, creates an
image-only VFS Docker store, and builds the `linux/amd64` VM image.
The standalone `remoteview.Dockerfile` supports both `linux/amd64` and
`linux/arm64` release binaries.

Export a Cocoon-importable rootfs:

```bash
cid="$(docker create local/ubuntu-redroid-gms-h264:22.04-android16)"
docker export "$cid" | gzip -9 > redroid16-gms-cocoon-amd64-rootfs.tar.gz
docker rm "$cid"
```

The rootfs export flattens the outer Dockerfile layers. Package lists, package
caches, build-only download tools, manuals, and transient logs are removed in
the image build. Cocoon then converts the tar stream to an `lz4hc`-compressed
EROFS layer during import; the gzip level only affects transfer size.

Then transfer the tarball to the offline host and run:

```bash
cocoon image import redroid16-gms:16.0-final \
  redroid16-gms-cocoon-amd64-rootfs.tar.gz
```

## Architecture boundary

The `scrcpy-rfb` server is released for both amd64 and arm64, and the Ubuntu VM
build is architecture-selectable. The Android payload in this directory is
currently the x86_64-only ReDroid 16 image with x86_64 GApps and ARM app
translation, so the complete image produced here is amd64. An arm64 Cocoon VM
uses the same service and rootfs design but still needs a pinned arm64 ReDroid
16 image, arm64 GApps overlay, and a guest kernel with the corresponding binder
modules.

## Verification

Run `verify-vm-inner.sh` inside the built rootfs/VM. Runtime checks should also
confirm `sys.boot_completed=1`, ports 5555/5900, Docker restart policy and data
mount, the enabled package set above, and concurrent H.264 plus ordinary RFB
clients.
