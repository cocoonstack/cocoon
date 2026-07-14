#!/bin/bash
# Create the ReDroid container once on the first VM boot, then idempotently make
# sure it is running. With Docker's vfs driver, deferring docker create keeps the
# full container rootfs copy out of the immutable Cocoon EROFS layer; the copy is
# paid once in the VM's writable COW layer. Later VM boots reuse the container
# and Docker's unless-stopped policy.
set -euo pipefail

REDROID_IMAGE="${REDROID_IMAGE:-local/redroid:vm}"

for _ in $(seq 1 120); do
    docker info >/dev/null 2>&1 && break
    sleep 1
done
docker info >/dev/null

if ! docker inspect redroid >/dev/null 2>&1; then
    docker image inspect "$REDROID_IMAGE" >/dev/null
    docker create --platform linux/amd64 --privileged \
        --name redroid --restart unless-stopped \
        -p 5555:5555 \
        -v /var/lib/redroid-data:/data \
        "$REDROID_IMAGE" \
        androidboot.use_memfd=0 \
        androidboot.redroid_width=720 \
        androidboot.redroid_height=1280 \
        androidboot.redroid_dpi=320 \
        androidboot.redroid_fps=60 \
        androidboot.redroid_gpu_mode=guest \
        androidboot.redroid_net_ndns=2 \
        androidboot.redroid_net_dns1=10.104.195.101 \
        androidboot.redroid_net_dns2=10.104.195.102 \
        ro.setupwizard.mode=DISABLED >/dev/null
fi

if [ "$(docker inspect -f '{{.State.Running}}' redroid)" != "true" ]; then
    docker start redroid >/dev/null
fi
