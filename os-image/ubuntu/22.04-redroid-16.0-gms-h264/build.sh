#!/bin/bash
# Build the Ubuntu 22.04 + Docker + ReDroid 16 GMS VM image with a baked,
# already-running container.
#
# A privileged DinD (vfs) loads the flattened ReDroid image, `docker run`s the
# container with `--restart unless-stopped`, then the DinD daemon is stopped
# gracefully so the container's running state + restart policy survive in
# /var/lib/docker. That whole store is baked into the VM image, so the VM's
# dockerd replays the container on boot with NO first-boot docker create/run —
# this removes the per-VM vfs create-copy from the run/start path.
#
# ReDroid must stay running in the DinD for a clean capture, so the BUILD HOST
# needs binder (+ ashmem for use_memfd=0) and must be the target ARCH natively
# (the container executes Android; no qemu emulation).
#
#   ARCH         amd64|arm64            (default amd64; must match the host arch)
#   REDROID_SRC  ReDroid image to bake  (default local/redroid-gms:16.0-cocoon)
#   REGISTRY     target namespace       (default docker.io/cmgs)
#   VM_TAG       VM image tag           (default ubuntu-redroid-16.0-gms-h264:22.04)
#   USE_MEMFD    redroid use_memfd      (default 1; portable — the bake host runs
#                                        the container and modern kernels drop
#                                        ashmem. use_memfd=1 also works on the
#                                        VM's 5.15 kernel)
#   PUSH         1=push, 0=load locally (default 1)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
UBUNTU_CTX="$(dirname "$HERE")"
ARCH="${ARCH:-amd64}"
REGISTRY="${REGISTRY:-docker.io/cmgs}"
VM_TAG="${VM_TAG:-ubuntu-redroid-16.0-gms-h264:22.04}"
REDROID_SRC="${REDROID_SRC:-local/redroid-gms:16.0-cocoon}"
DIND_IMAGE="${DIND_IMAGE:-docker:dind}"
USE_MEMFD="${USE_MEMFD:-1}"
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

# ReDroid needs binder (+ ashmem for use_memfd=0) on the build host to stay
# running in the DinD, so the baked container is captured Running for --restart
# replay on the VM.
sudo modprobe binder_linux devices="binder,hwbinder,vndbinder" 2>/dev/null || true
[ "$USE_MEMFD" = 0 ] && sudo modprobe ashmem_linux 2>/dev/null || true

echo ">> baking a running ReDroid container into /var/lib/docker (vfs) via DinD"
docker rm -f rd-gen >/dev/null 2>&1 || true
docker volume rm rd-vld >/dev/null 2>&1 || true
docker run -d --privileged --name rd-gen \
    -v "$HERE/daemon.json":/etc/docker/daemon.json:ro \
    -v "$HERE/redroid-image.tar":/redroid.tar:ro \
    -v rd-vld:/var/lib/docker \
    "$DIND_IMAGE" >/dev/null
docker exec -e REDROID_SRC="$REDROID_SRC" -e USE_MEMFD="$USE_MEMFD" rd-gen sh -c '
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
        ro.setupwizard.mode=DISABLED >/dev/null
    sleep 30
    if [ "$(docker inspect -f "{{.State.Running}}" redroid)" != true ]; then
        echo "baked ReDroid container is not Running (build host lacks binder/ashmem?)" >&2
        docker logs redroid 2>&1 | tail -40 >&2
        exit 1
    fi
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
