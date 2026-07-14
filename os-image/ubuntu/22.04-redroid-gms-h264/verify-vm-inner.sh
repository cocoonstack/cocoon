#!/bin/bash
# Static checks for the built Ubuntu VM image. Run inside its root filesystem.
set -euo pipefail

test -e /boot/vmlinuz
test -e /boot/initrd.img
test -d /var/lib/redroid-data
test -f /lib/modules/*/kernel/drivers/staging/android/ashmem_linux.ko
test -f /lib/modules/*/kernel/drivers/android/binder_linux.ko
test -f /etc/systemd/system/redroid.service
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

systemctl is-enabled docker.service containerd.service redroid.service remoteview.service >/dev/null
dockerd --validate --config-file=/etc/docker/daemon.json >/dev/null
grep -Fq '"storage-driver": "vfs"' /etc/docker/daemon.json
grep -q binder_linux /etc/modules-load.d/redroid.conf
grep -q '^Requires=docker.service$' /etc/systemd/system/redroid.service
grep -q '^After=docker.service$' /etc/systemd/system/redroid.service
grep -Fqx 'TimeoutStartSec=0' /etc/systemd/system/redroid.service
grep -Fqx 'ExecStartPre=/sbin/modprobe ashmem_linux' /etc/systemd/system/redroid.service
grep -Fqx 'ExecStartPre=/sbin/modprobe binder_linux devices=binder,hwbinder,vndbinder' /etc/systemd/system/redroid.service
grep -q '^Requires=redroid.service$' /etc/systemd/system/remoteview.service
grep -q '^After=redroid.service$' /etc/systemd/system/remoteview.service

shopt -s nullglob
configs=(/var/lib/docker/containers/*/config.v2.json)
hosts=(/var/lib/docker/containers/*/hostconfig.json)
test "${#configs[@]}" -eq 0
test "${#hosts[@]}" -eq 0
grep -Fq 'local/redroid' /var/lib/docker/image/vfs/repositories.json
grep -Fq 'docker create --platform linux/amd64 --privileged' /usr/local/sbin/redroid-ensure.sh
grep -Fq -- '--name redroid --restart unless-stopped' /usr/local/sbin/redroid-ensure.sh
grep -Fq -- '-p 5555:5555' /usr/local/sbin/redroid-ensure.sh
grep -Fq -- '-v /var/lib/redroid-data:/data' /usr/local/sbin/redroid-ensure.sh

for value in \
    androidboot.use_memfd=0 \
    androidboot.redroid_fps=60 \
    androidboot.redroid_width=720 \
    androidboot.redroid_height=1280 \
    androidboot.redroid_dpi=320 \
    androidboot.redroid_gpu_mode=guest \
    androidboot.redroid_net_dns1=10.104.195.101 \
    androidboot.redroid_net_dns2=10.104.195.102 \
    ro.setupwizard.mode=DISABLED; do
    grep -Fq "$value" /usr/local/sbin/redroid-ensure.sh
done

echo vm-static-check=ok
