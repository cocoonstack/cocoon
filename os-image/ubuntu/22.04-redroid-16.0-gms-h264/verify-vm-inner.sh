#!/bin/bash
# Static checks for the built Ubuntu VM image. Run inside its root filesystem.
set -euo pipefail

test -e /boot/vmlinuz
test -e /boot/initrd.img
test -d /var/lib/redroid-data
KVER="$(ls /lib/modules | sort -V | tail -1)"
modinfo -k "$KVER" ashmem_linux >/dev/null
modinfo -k "$KVER" binder_linux >/dev/null
test -f /etc/systemd/system/remoteview.service
grep -Fq 'MAX_FPS="${MAX_FPS:-60}"' /usr/local/bin/remoteview-run.sh
grep -Fq 'VIDEO_BIT_RATE="${VIDEO_BIT_RATE:-8000000}"' /usr/local/bin/remoteview-run.sh
grep -Fq -- '-rfbport "$RFB_PORT"' /usr/local/bin/remoteview-run.sh
grep -Fq 'video_codec=h264' /usr/local/bin/remoteview-run.sh
command -v scrcpy-rfb >/dev/null
! command -v fuse-overlayfs >/dev/null
test -f /usr/local/share/scrcpy/scrcpy-server.jar
! ldd /usr/local/bin/scrcpy-rfb | grep -q 'not found'
! command -v Xvfb >/dev/null
! command -v x11vnc >/dev/null
! command -v Xtigervnc >/dev/null
! command -v openbox >/dev/null
! command -v scrcpy >/dev/null

# No first-boot launcher: the container is baked already-created with a restart
# policy, so dockerd replays it on boot instead of a service running docker create.
test ! -e /etc/systemd/system/redroid.service
test ! -e /usr/local/sbin/redroid-ensure.sh

systemctl is-enabled docker.service containerd.service remoteview.service >/dev/null
dockerd --validate --config-file=/etc/docker/daemon.json >/dev/null
grep -Fq '"storage-driver": "vfs"' /etc/docker/daemon.json
grep -q binder_linux /etc/modules-load.d/redroid.conf
grep -q ashmem_linux /etc/modules-load.d/redroid.conf
# systemd-modules-load is skipped under container-detected virt, so binder/ashmem
# load via a docker ExecStartPre drop-in before dockerd replays the container.
test -f /etc/systemd/system/docker.service.d/redroid-modules.conf
grep -Fq 'modprobe binder_linux' /etc/systemd/system/docker.service.d/redroid-modules.conf
grep -Fq 'modprobe ashmem_linux' /etc/systemd/system/docker.service.d/redroid-modules.conf
grep -q '^Requires=docker.service$' /etc/systemd/system/remoteview.service
grep -q '^After=docker.service$' /etc/systemd/system/remoteview.service

# networkd must leave docker's veth/bridge alone; otherwise it races docker's
# enslavement and strands the container's veth off docker0 (no container net).
test -f /etc/systemd/network/05-docker-unmanaged.network
grep -Fq 'Unmanaged=yes' /etc/systemd/network/05-docker-unmanaged.network
grep -Eq 'Name=.*veth\*' /etc/systemd/network/05-docker-unmanaged.network

# Baked ReDroid container: exactly one, name=redroid, restart=unless-stopped,
# 5555 published, /data bind, and the androidboot args in its command.
shopt -s nullglob
configs=(/var/lib/docker/containers/*/config.v2.json)
test "${#configs[@]}" -eq 1
cfg="${configs[0]}"
host="$(dirname "$cfg")/hostconfig.json"
test -f "$host"
grep -Fq '"Name":"/redroid"' "$cfg"
grep -Fq '"Name":"unless-stopped"' "$host"
grep -Fq '"5555/tcp"' "$host"
grep -Fq '/var/lib/redroid-data:/data' "$host"
grep -Fq 'local/redroid' /var/lib/docker/image/vfs/repositories.json
grep -Fq 'androidboot.redroid_net_ndns=2' "$cfg"
grep -Fq 'androidboot.redroid_net_dns1=' "$cfg"
grep -Fq 'androidboot.redroid_net_dns2=' "$cfg"
! grep -Eq 'androidboot\.redroid_net_dns[12]=10\.104\.' "$cfg"

for value in \
    androidboot.use_memfd=0 \
    androidboot.redroid_fps=60 \
    androidboot.redroid_width=720 \
    androidboot.redroid_height=1280 \
    androidboot.redroid_dpi=320 \
    androidboot.redroid_gpu_mode=guest \
    ro.setupwizard.mode=DISABLED; do
    grep -Fq "$value" "$cfg"
done

echo vm-static-check=ok
