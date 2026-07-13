#!/bin/bash
# Local build + push for the Ubuntu+Docker+Redroid VM image.
#
# The redroid container is created AND started at BUILD time inside a privileged
# DinD, then its /var/lib/docker (image + --restart unless-stopped container) is
# baked into the VM image, so the VM's dockerd auto-runs redroid on boot with no
# docker run at first start. amd64 only; run on a native amd64 docker host.
#
#   REDROID_SRC  redroid image to bake  (default plain redroid 15)
#   REGISTRY     target namespace       (default docker.io/cmgs)
#   VM_TAG       VM image tag           (default ubuntu-redroid:24.04)
#   PUSH         1=push, 0=load locally (default 1)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
UBUNTU_CTX="$(dirname "$HERE")"
REGISTRY="${REGISTRY:-docker.io/cmgs}"
VM_TAG="${VM_TAG:-ubuntu-redroid:24.04}"
REDROID_SRC="${REDROID_SRC:-redroid/redroid:15.0.0_64only-latest}"
DIND_IMAGE="${DIND_IMAGE:-docker:dind}"
PUSH="${PUSH:-1}"
PLATFORM="linux/amd64"

cleanup() {
    docker rm -f rd-gen >/dev/null 2>&1 || true
    docker volume rm rd-vld >/dev/null 2>&1 || true
    rm -f "$HERE/redroid-image.tar" "$HERE/docker-data.tar"
}
trap cleanup EXIT

docker image inspect "$REDROID_SRC" >/dev/null 2>&1 || docker pull "$REDROID_SRC"
docker save "$REDROID_SRC" -o "$HERE/redroid-image.tar"

# redroid needs binder on the build host to boot cleanly inside the DinD, so the
# baked container is captured in a stable "running" state for --restart replay.
sudo modprobe binder_linux devices="binder,hwbinder,vndbinder" 2>/dev/null || true

echo ">> generating /var/lib/docker (build-time redroid run) via DinD"
docker rm -f rd-gen >/dev/null 2>&1 || true
docker volume rm rd-vld >/dev/null 2>&1 || true
docker run -d --privileged --name rd-gen \
    -v "$HERE/daemon.json":/etc/docker/daemon.json:ro \
    -v "$HERE/redroid-image.tar":/redroid.tar:ro \
    -v rd-vld:/var/lib/docker \
    "$DIND_IMAGE" >/dev/null
docker exec rd-gen sh -c '
    set -e
    for i in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
    docker info --format "storage-driver={{.Driver}}"
    docker load -i /redroid.tar
    IMG=$(docker images --format "{{.Repository}}:{{.Tag}}" | grep -i redroid | head -1)
    docker run -d --privileged --name redroid --restart unless-stopped \
        -p 5555:5555 -v /var/lib/redroid-data:/data \
        "$IMG" androidboot.use_memfd=1 androidboot.redroid_fps=60
    sleep 20
    docker ps --format "baked: {{.Names}} {{.Status}}"
'
# Graceful daemon stop (not kill): dockerd stops the container but keeps its
# restart-policy state, so the VM's dockerd replays it. dockerd is PID 1 here, so
# tar the data volume from a separate container, not from inside a killed DinD.
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
    --secret id=cocoon_overlay,src="$UBUNTU_CTX/overlay.sh" \
    --secret id=cocoon_network,src="$UBUNTU_CTX/network.sh" \
    --secret id=cocoon_install_agent,src="$UBUNTU_CTX/install-agent.sh" \
    --secret id=daemon_json,src="$HERE/daemon.json" \
    "$UBUNTU_CTX"
echo ">> done: $REGISTRY/$VM_TAG"
