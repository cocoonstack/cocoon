#!/system/bin/sh
# Prime Docker's restart policy during the image bake without booting Android
# on the CI host. Removing the marker changes only this container's VFS root;
# when dockerd replays the baked container in the VM, the marker is already
# absent and Android /init starts immediately.
set -eu

if [ -e /.cocoon-bake-once ]; then
    /busybox rm -f /.cocoon-bake-once
    /busybox sleep 600
    exit 0
fi

exec /init "$@"
