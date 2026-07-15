#!/bin/bash
# Bake a restart-primed ReDroid 16 GMS container into docker-data-<arch>.tar.
#
# A privileged DinD (vfs) loads the flattened ReDroid image and starts its
# one-shot bake wrapper for long enough to arm Docker's restart policy. The
# wrapper removes a marker from this container's VFS root but does not boot
# Android on the build host. DinD is then stopped gracefully so the created
# container + restart state survive in /var/lib/docker, which is hardlink-deduped
# and tarred to docker-data-<arch>.tar. The VM's Dockerfile ADDs that tar; the VM
# dockerd replays the container and the marker-free wrapper execs /init.
#
#   ARCH         amd64|arm64            (default amd64; the REDROID_SRC arch)
#   REDROID_SRC  ReDroid image to bake  (default local/redroid-gms:16.0-cocoon)
#   USE_MEMFD    redroid use_memfd      (default 0; Android 16 still creates
#                                        ashmem in system_server, and the VM's
#                                        Jammy kernel provides ashmem_linux)
#   REDROID_DNS1 first Android DNS       (default 8.8.8.8; baked into container)
#   REDROID_DNS2 second Android DNS      (default 1.1.1.1; baked into container)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARCH="${ARCH:-amd64}"
REDROID_SRC="${REDROID_SRC:-local/redroid-gms:16.0-cocoon}"
DIND_IMAGE="${DIND_IMAGE:-docker:dind}"
USE_MEMFD="${USE_MEMFD:-0}"
REDROID_DNS1="${REDROID_DNS1:-8.8.8.8}"
REDROID_DNS2="${REDROID_DNS2:-1.1.1.1}"
PLATFORM="linux/$ARCH"
case "$ARCH" in amd64|arm64) ;; *) echo "unsupported ARCH: $ARCH" >&2; exit 1 ;; esac

cleanup() {
    docker rm -f rd-gen >/dev/null 2>&1 || true
    docker volume rm rd-vld >/dev/null 2>&1 || true
    rm -f "$HERE/redroid-image.tar"
}
trap cleanup EXIT

docker image inspect "$REDROID_SRC" >/dev/null 2>&1 || docker pull --platform "$PLATFORM" "$REDROID_SRC"
test "$(docker image inspect --format '{{len .RootFS.Layers}}' "$REDROID_SRC")" = 1 || {
    echo "ReDroid source must be flattened to one layer for the vfs image store" >&2
    exit 1
}
docker save "$REDROID_SRC" -o "$HERE/redroid-image.tar"

echo ">> baking a restart-primed ReDroid container into /var/lib/docker (vfs) via DinD"
docker rm -f rd-gen >/dev/null 2>&1 || true
docker volume rm rd-vld >/dev/null 2>&1 || true
docker run -d --privileged --name rd-gen \
    -v "$HERE/daemon.json":/etc/docker/daemon.json:ro \
    -v "$HERE/redroid-image.tar":/redroid.tar:ro \
    -v rd-vld:/var/lib/docker \
    "$DIND_IMAGE" >/dev/null
docker exec \
    -e REDROID_SRC="$REDROID_SRC" \
    -e USE_MEMFD="$USE_MEMFD" \
    -e REDROID_DNS1="$REDROID_DNS1" \
    -e REDROID_DNS2="$REDROID_DNS2" \
    rd-gen sh -c '
    set -e
    for i in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
    test "$(docker info --format "{{.Driver}}")" = vfs
    # Register qemu binfmt (F-flag) inside THIS docker daemon so a cross-arch
    # ReDroid image (e.g. arm64 baked on an amd64 runner) can run the sleep
    # wrapper; the host registration is not always visible to a nested daemon.
    docker run --privileged --rm tonistiigi/binfmt --install all >/dev/null 2>&1 || true
    docker load -i /redroid.tar
    docker tag "$REDROID_SRC" local/redroid:vm
    [ "$REDROID_SRC" = local/redroid:vm ] || docker image rm "$REDROID_SRC" >/dev/null
    docker run -d --privileged --name redroid --restart unless-stopped \
        -p 5555:5555 -v /var/lib/redroid-data:/data \
        local/redroid:vm \
        androidboot.use_memfd=$USE_MEMFD \
        androidboot.redroid_width=720 \
        androidboot.redroid_height=1280 \
        androidboot.redroid_dpi=320 \
        androidboot.redroid_fps=60 \
        androidboot.redroid_gpu_mode=guest \
        androidboot.redroid_net_ndns=2 \
        "androidboot.redroid_net_dns1=$REDROID_DNS1" \
        "androidboot.redroid_net_dns2=$REDROID_DNS2" \
        ro.setupwizard.mode=DISABLED >/dev/null
    # Docker only activates a restart policy after a container has stayed up
    # for 10 seconds. The bake wrapper sleeps without starting Android.
    sleep 20
    state="$(docker inspect -f "{{.State.Running}} {{.RestartCount}}" redroid 2>&1)"
    if [ "$state" != "true 0" ]; then
        echo "baked ReDroid wrapper not cleanly Running (state=$state)" >&2
        docker logs redroid 2>&1 | tail -60 >&2
        exit 1
    fi
    docker exec redroid /busybox test ! -e /.cocoon-bake-once
    docker ps --format "baked: {{.Names}} {{.Status}}"
'
# Graceful daemon stop (not kill): dockerd stops the container but keeps its
# restart-policy state, so the VM dockerd replays it. dockerd is PID 1 here, so
# tar the data volume from a separate container, not from inside the stopped DinD.
docker stop -t 40 rd-gen >/dev/null
# vfs stores the ReDroid image layer, the container, and its init layer as three
# near-identical full copies (~1.3G each). Hardlink the identical files before tar
# so the image ships them once. Safe: in the VM /var/lib/docker is a read-only
# EROFS lower under overlayfs, so any container write copies up and never mutates
# a shared inode.
docker run --rm -e ARCH="$ARCH" -v rd-vld:/vld -v "$HERE":/out ubuntu bash -c '
    command -v hardlink >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq util-linux >/dev/null; }
    hardlink /vld/vfs/dir
    tar --numeric-owner -C /vld -cf "/out/docker-data-$ARCH.tar" .'
docker rm -f rd-gen >/dev/null
docker volume rm rd-vld >/dev/null
echo ">> docker-data-$ARCH.tar $(du -h "$HERE/docker-data-$ARCH.tar" | cut -f1)"
