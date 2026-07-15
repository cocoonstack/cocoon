#!/bin/bash
# CI build-artifact hook for os-image/ubuntu/22.04-redroid-16.0-gms-h264.
# build-os-images.yml runs this (only when present) after secret discovery and
# before build-push-action. Per arch it carves the GApps overlay, builds the
# flattened ReDroid+GMS source image, and bakes a restart-primed container into
# docker-data-<arch>.tar — the artifact the Dockerfile's per-arch ADD consumes.
# The job's Set up QEMU step lets the arm64 leg run on the amd64 runner: the bake
# wrapper only arms the restart policy and sleeps, it never boots Android, so no
# native arch or binder is needed.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
    curl ca-certificates unzip gzip erofs-utils android-sdk-libsparse-utils e2fsprogs

for entry in amd64:x86_64 arm64:arm64-v8a; do
    ARCH="${entry%%:*}"
    ABI="${entry##*:}"
    echo "==== ci-prepare: $ARCH ($ABI) ===="

    ARCH="$ARCH" bash prepare-gapps16.sh
    test -f "gapps16-${ABI}.tar"

    docker buildx build --load --platform "linux/$ARCH" \
        --build-arg TARGETARCH="$ARCH" \
        --build-arg GAPPS_TAR="gapps16-${ABI}.tar" \
        -f redroid-gms16.Dockerfile \
        -t "local/redroid-gms:16.0-cocoon-$ARCH" .
    test "$(docker image inspect --format '{{len .RootFS.Layers}}' \
        "local/redroid-gms:16.0-cocoon-$ARCH")" = 1

    ARCH="$ARCH" USE_MEMFD=0 \
        REDROID_SRC="local/redroid-gms:16.0-cocoon-$ARCH" \
        bash build.sh
    test -f "docker-data-${ARCH}.tar"

    # Bound peak disk before the next arch: the gapps tar and source image are
    # fully consumed; only docker-data-<arch>.tar must survive to the build step.
    rm -f "gapps16-${ABI}.tar"
    docker image rm "local/redroid-gms:16.0-cocoon-$ARCH" >/dev/null 2>&1 || true
    docker image prune -f >/dev/null 2>&1 || true
done

ls -lh docker-data-amd64.tar docker-data-arm64.tar
