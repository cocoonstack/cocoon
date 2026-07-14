#!/bin/bash
# Cross-build Ubuntu + Docker + ReDroid 16 for Cocoon from an arm64 Mac.
# ReDroid is loaded into a vfs DinD store, but its container is deliberately not
# created on the build host. docker create is deferred to the first VM boot.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
UBUNTU_CTX="$(dirname "$HERE")"
REDROID_SRC="${REDROID_SRC:-local/redroid-gms:16.0-cocoon}"
VM_IMAGE="${VM_IMAGE:-local/ubuntu-redroid-gms:22.04-android16}"
DIND_IMAGE="${DIND_IMAGE:-docker:29.4-dind}"
PLATFORM=linux/amd64
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
    rm -f "$HERE/redroid-image.tar"
}
trap cleanup EXIT

docker image inspect "$REDROID_SRC" >/dev/null
test "$(docker image inspect --format '{{len .RootFS.Layers}}' "$REDROID_SRC")" = 1 || {
    echo "ReDroid source must be flattened to one layer for the vfs image store" >&2
    exit 1
}
docker save "$REDROID_SRC" -o "$HERE/redroid-image.tar"

docker rm -f rd-gen >/dev/null 2>&1 || true
docker volume rm rd-vld >/dev/null 2>&1 || true
docker run -d --privileged --name rd-gen \
    -v "$HERE/daemon.json":/etc/docker/daemon.json:ro \
    -v "$HERE/redroid-image.tar":/redroid.tar:ro \
    -v rd-vld:/var/lib/docker \
    "$DIND_IMAGE" >/dev/null

docker exec -e REDROID_SRC="$REDROID_SRC" rd-gen sh -c '
    set -eu
    for i in $(seq 1 120); do docker info >/dev/null 2>&1 && break; sleep 1; done
    docker info >/dev/null
    test "$(docker info --format "{{.Driver}}")" = vfs
    docker load -i /redroid.tar
    test "$(docker image inspect --format "{{len .RootFS.Layers}}" "$REDROID_SRC")" = 1
    docker tag "$REDROID_SRC" local/redroid:vm
    if [ "$REDROID_SRC" != local/redroid:vm ]; then
        docker image rm "$REDROID_SRC" >/dev/null
    fi
    test -z "$(docker container ls -aq)"
    docker builder prune -af >/dev/null
    docker image inspect local/redroid:vm --format "baked: {{.Id}} layers={{len .RootFS.Layers}}"
'

docker stop -t 20 rd-gen >/dev/null
docker run --rm -v rd-vld:/vld -v "$HERE":/out ubuntu:24.04 \
    tar --numeric-owner --xattrs -C /vld -cf /out/docker-data.tar .
echo "docker-data.tar: $(du -h "$HERE/docker-data.tar" | cut -f1)"
docker rm -f rd-gen >/dev/null
docker volume rm rd-vld >/dev/null

docker buildx build --platform "$PLATFORM" --load \
    -f "$HERE/Dockerfile" -t "$VM_IMAGE" \
    --build-arg SCRCPY_RFB_REPO="$SCRCPY_RFB_REPO" \
    --build-arg SCRCPY_RFB_TAG="$SCRCPY_RFB_TAG" \
    --build-arg SCRCPY_RFB_COMMIT="$SCRCPY_RFB_COMMIT" \
    --secret id=cocoon_overlay,src="$UBUNTU_CTX/overlay.sh" \
    --secret id=cocoon_network,src="$UBUNTU_CTX/network.sh" \
    --secret id=cocoon_install_agent,src="$UBUNTU_CTX/install-agent.sh" \
    --secret id=daemon_json,src="$HERE/daemon.json" \
    "$UBUNTU_CTX"

echo "built: $VM_IMAGE (scrcpy-rfb $SCRCPY_RFB_COMMIT)"
