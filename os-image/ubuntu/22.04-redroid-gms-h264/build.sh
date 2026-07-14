#!/bin/bash
# Local build + push for the Ubuntu+Docker+Redroid VM image.
#
# The flattened ReDroid image is loaded into a vfs DinD store. The container is
# created once on the VM's first boot so its full vfs rootfs copy lands in the
# writable Cocoon COW rather than the immutable EROFS image.
#
#   REDROID_SRC  redroid image to bake  (default plain redroid 15)
#   REGISTRY     target namespace       (default docker.io/cmgs)
#   VM_TAG       VM image tag           (default ubuntu-redroid-gms-h264:22.04)
#   PUSH         1=push, 0=load locally (default 1)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
UBUNTU_CTX="$(dirname "$HERE")"
REGISTRY="${REGISTRY:-docker.io/cmgs}"
VM_TAG="${VM_TAG:-ubuntu-redroid-gms-h264:22.04}"
REDROID_SRC="${REDROID_SRC:-local/redroid-gms:16.0-cocoon}"
DIND_IMAGE="${DIND_IMAGE:-docker:dind}"
PUSH="${PUSH:-1}"
PLATFORM="linux/amd64"
SCRCPY_RFB_REPO="${SCRCPY_RFB_REPO:-cocoonstack/libvncserver}"
SCRCPY_RFB_TAG="${SCRCPY_RFB_TAG:-dev}"
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

docker image inspect "$REDROID_SRC" >/dev/null 2>&1 || docker pull "$REDROID_SRC"
test "$(docker image inspect --format '{{len .RootFS.Layers}}' "$REDROID_SRC")" = 1 || {
    echo "ReDroid source must be flattened to one layer for the vfs image store" >&2
    exit 1
}
docker save "$REDROID_SRC" -o "$HERE/redroid-image.tar"

echo ">> generating image-only /var/lib/docker with vfs via DinD"
docker rm -f rd-gen >/dev/null 2>&1 || true
docker volume rm rd-vld >/dev/null 2>&1 || true
docker run -d --privileged --name rd-gen \
    -v "$HERE/daemon.json":/etc/docker/daemon.json:ro \
    -v "$HERE/redroid-image.tar":/redroid.tar:ro \
    -v rd-vld:/var/lib/docker \
    "$DIND_IMAGE" >/dev/null
docker exec -e REDROID_SRC="$REDROID_SRC" rd-gen sh -c '
    set -e
    for i in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
    docker info --format "storage-driver={{.Driver}}"
    test "$(docker info --format "{{.Driver}}")" = vfs
    docker load -i /redroid.tar
    docker tag "$REDROID_SRC" local/redroid:vm
    if [ "$REDROID_SRC" != local/redroid:vm ]; then
        docker image rm "$REDROID_SRC" >/dev/null
    fi
    test -z "$(docker container ls -aq)"
    docker image inspect local/redroid:vm --format "baked: {{.Id}} layers={{len .RootFS.Layers}}"
'
# Gracefully stop dockerd before archiving its image-only graphdriver state.
docker stop -t 40 rd-gen >/dev/null
docker run --rm -v rd-vld:/vld -v "$HERE":/out ubuntu \
    tar --numeric-owner -C /vld -cf /out/docker-data.tar .
docker rm -f rd-gen >/dev/null
docker volume rm rd-vld >/dev/null
echo ">> docker-data.tar $(du -h "$HERE/docker-data.tar" | cut -f1)"

echo ">> building $REGISTRY/$VM_TAG"
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
