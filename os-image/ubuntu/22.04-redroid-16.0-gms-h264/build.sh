#!/bin/bash
# Build the Ubuntu 22.04 + Docker + ReDroid 16 GMS VM image with a baked,
# restart-primed container.
#
# A privileged DinD (vfs) loads the flattened ReDroid image and starts its
# one-shot bake wrapper for long enough to arm Docker's restart policy. The
# wrapper removes a marker from this container's VFS root but does not boot
# Android on the build host. DinD is then stopped gracefully so the created
# container + restart state survive in /var/lib/docker. In the VM, dockerd
# replays that container and the marker-free wrapper immediately execs /init:
# no first-boot docker create/run and no per-VM vfs create-copy.
#
#   ARCH         amd64|arm64            (default amd64; must match the host arch)
#   REDROID_SRC  ReDroid image to bake  (default local/redroid-gms:16.0-cocoon)
#   REGISTRY     target namespace       (default docker.io/cmgs)
#   VM_TAG       VM image tag           (default ubuntu-redroid-16.0-gms-h264:22.04)
#   USE_MEMFD    redroid use_memfd      (default 0; Android 16 still creates
#                                        ashmem in system_server, and the VM's
#                                        Jammy kernel provides ashmem_linux)
#   REDROID_DNS1 first Android DNS       (default 8.8.8.8; baked into container)
#   REDROID_DNS2 second Android DNS      (default 1.1.1.1; baked into container)
#   PUSH         1=push, 0=load locally (default 1)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
UBUNTU_CTX="$(dirname "$HERE")"
ARCH="${ARCH:-amd64}"
REGISTRY="${REGISTRY:-docker.io/cmgs}"
VM_TAG="${VM_TAG:-ubuntu-redroid-16.0-gms-h264:22.04}"
REDROID_SRC="${REDROID_SRC:-local/redroid-gms:16.0-cocoon}"
DIND_IMAGE="${DIND_IMAGE:-docker:dind}"
USE_MEMFD="${USE_MEMFD:-0}"
REDROID_DNS1="${REDROID_DNS1:-8.8.8.8}"
REDROID_DNS2="${REDROID_DNS2:-1.1.1.1}"
PUSH="${PUSH:-1}"
PLATFORM="linux/$ARCH"
SCRCPY_RFB_REPO="${SCRCPY_RFB_REPO:-cocoonstack/libvncserver}"
SCRCPY_RFB_TAG="${SCRCPY_RFB_TAG:-dev}"
case "$ARCH" in amd64|arm64) ;; *) echo "unsupported ARCH: $ARCH" >&2; exit 1 ;; esac
if [ -z "${SCRCPY_RFB_COMMIT:-}" ]; then
    SCRCPY_RFB_COMMIT="$(
        curl -fsSL \
          "https://github.com/${SCRCPY_RFB_REPO}/releases/download/${SCRCPY_RFB_TAG}/build-info.json" \
        | sed -n 's/.*"commit": "\([0-9a-f]\{40\}\)".*/\1/p'
    )"
fi
if [ "${#SCRCPY_RFB_COMMIT}" -ne 40 ]; then
    echo "invalid scrcpy-rfb release commit: $SCRCPY_RFB_COMMIT" >&2
    exit 1
fi
case "$SCRCPY_RFB_COMMIT" in
    *[!0-9a-f]*) echo "invalid scrcpy-rfb release commit: $SCRCPY_RFB_COMMIT" >&2; exit 1 ;;
esac

cleanup() {
    docker rm -f rd-gen >/dev/null 2>&1 || true
    docker volume rm rd-vld >/dev/null 2>&1 || true
    rm -f "$HERE/redroid-image.tar" "$HERE/docker-data.tar"
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
    if [ "$(docker inspect -f "{{.State.Running}}" redroid)" != true ]; then
        echo "baked ReDroid restart-policy wrapper is not Running" >&2
        docker logs redroid 2>&1 | tail -40 >&2
        exit 1
    fi
    docker exec redroid /busybox test ! -e /.cocoon-bake-once
    docker ps --format "baked: {{.Names}} {{.Status}}"
'
# Graceful daemon stop (not kill): dockerd stops the container but keeps its
# restart-policy state, so the VM dockerd replays it. dockerd is PID 1 here, so
# tar the data volume from a separate container, not from inside the stopped DinD.
docker stop -t 40 rd-gen >/dev/null
docker run --rm -v rd-vld:/vld -v "$HERE":/out ubuntu \
    tar --numeric-owner -C /vld -cf /out/docker-data.tar .
docker rm -f rd-gen >/dev/null
docker volume rm rd-vld >/dev/null
echo ">> docker-data.tar $(du -h "$HERE/docker-data.tar" | cut -f1)"

echo ">> building $REGISTRY/$VM_TAG ($PLATFORM)"
OUT=$([ "$PUSH" = 1 ] && echo --push || echo --load)
docker buildx build --platform "$PLATFORM" "$OUT" \
    -f "$HERE/Dockerfile" -t "$REGISTRY/$VM_TAG" \
    --build-arg SCRCPY_RFB_REPO="$SCRCPY_RFB_REPO" \
    --build-arg SCRCPY_RFB_TAG="$SCRCPY_RFB_TAG" \
    --build-arg SCRCPY_RFB_COMMIT="$SCRCPY_RFB_COMMIT" \
    --secret id=cocoon_overlay,src="$UBUNTU_CTX/overlay.sh" \
    --secret id=cocoon_network,src="$UBUNTU_CTX/network.sh" \
    --secret id=cocoon_install_agent,src="$UBUNTU_CTX/install-agent.sh" \
    --secret id=daemon_json,src="$HERE/daemon.json" \
    "$UBUNTU_CTX"
echo ">> done: $REGISTRY/$VM_TAG"
