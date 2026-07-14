# Ubuntu 24.04 + Docker + Redroid (`24.04-redroid`)

A self-contained VM image that boots Ubuntu, auto-starts Docker, and runs a
[Redroid](https://github.com/remote-android/redroid-doc) Android container so an
external client reaches Android over `adb connect <vm-ip>:5555`.

`linux/amd64` only (redroid and the ARM translator are x86_64).

## How it works

- **Docker engine** (latest stable), `systemctl enable`d. Storage driver is
  **vfs** with the containerd image store disabled — the cocoon rootfs is
  overlayfs, on which docker's `overlay2` is kernel-refused.
- **Redroid is created and started at BUILD time** (`build.sh`, in a privileged
  DinD) with `--restart unless-stopped -p 5555:5555`, and its `/var/lib/docker`
  (image + container) is baked into the VM image. On boot the VM's dockerd
  replays the restart policy and brings redroid up itself — **no docker run and
  no launcher service at first VM start**.
- **Bridge networking, not `--network host`**: under host net redroid's Android
  `netd` takes over the VM's shared netns and installs its policy routing
  (`32000: from all unreachable`), which black-holes the VM's own return path
  and makes the VM unreachable from outside. Bridge mode confines `netd` to the
  container netns; docker DNATs the VM's `:5555` to redroid's `adbd`.
- **binder**: the VM kernel is redroid's host kernel, so it ships
  `linux-modules-extra` (binder_linux + the netfilter tables docker/netd need)
  and loads `binder_linux devices="binder,hwbinder,vndbinder"` via
  `modules-load.d` before docker starts.

## Build

Run on a **native amd64** docker host (not qemu emulation) with `binder_linux`
available, logged in to the target registry:

```bash
# plain redroid 15:
bash build.sh
# redroid + GMS (build cmgs/redroid-gms:15.0 first, then REDROID_SRC=... build.sh)
```

`build.sh` starts redroid once inside a DinD to capture a running container,
then bakes `/var/lib/docker` into the image. `redroid-image.tar` and
`docker-data.tar` are generated during the build and git-ignored. See `build.sh`
for the env-var contract.

## Run

```bash
cocoon vm run --name redroid --cpu 4 --memory 4G --storage 32G <registry>/ubuntu-redroid:24.04
adb connect <vm-ip>:5555
```

`--storage` must leave room for the vfs container layer in the COW (redroid is a
few GB; vfs multiplies it across layers).

## Caveats

- **vfs is space-heavy**: the baked `/var/lib/docker` and each container layer
  are full copies, so the image and COW are large. This is the cost of running
  docker on an overlayfs rootfs.
- **GMS (M2) is pending hardware verification** — see the note atop
  `redroid-gms.Dockerfile`.
- **Not built by CI yet**: the baked tar is produced by `build.sh` (needs a
  privileged DinD + binder), not tracked in git, so
  `.github/workflows/build-os-images.yml` cannot build this tag as-is. CI
  integration is a follow-up.
