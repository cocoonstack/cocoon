#!/bin/sh
# Filename: overlay.sh
# Target path: /etc/initramfs-tools/scripts/cocoon-overlay

. /scripts/functions

resolve_disk() {
    local serial="$1" timeout="${COCOON_TIMEOUT:-10}" i=0
    case "$timeout" in ''|*[!0-9]*) timeout=10 ;; esac

    # Direct device path (Firecracker uses /dev/vdX, no virtio serial support)
    case "$serial" in
        /dev/*)
            while [ $i -lt $timeout ]; do
                [ -b "$serial" ] && echo "$serial" && return 0
                sleep 1
                i=$((i + 1))
            done
            return 1
            ;;
    esac

    # Serial name lookup (Cloud Hypervisor virtio-blk serial)
    while [ $i -lt $timeout ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            local s=""
            [ -f "$sysdev/serial" ] && s=$(cat "$sysdev/serial")
            [ -f "$sysdev/device/serial" ] && s=$(cat "$sysdev/device/serial")

            # Trim trailing whitespace
            while :; do case "$s" in *[[:space:]]) s="${s%[[:space:]]}" ;; *) break ;; esac; done

            if [ "$s" = "$serial" ]; then
                echo "/dev/${sysdev##*/}"
                return 0
            fi
        done
        sleep 1
        i=$((i + 1))
    done
    return 1
}

mountroot() {
    log_begin_msg "Cocoon: mounting stealth overlay rootfs"

    # Process kernel ip= parameters if present (creates /run/net-*.conf).
    # Only call configure_networking when ip= is on the cmdline — without it,
    # the function probes for devices and waits for udev, adding delay on
    # VMs with no NICs.
    if ! ls /run/net-*.conf >/dev/null 2>&1; then
        for _x in $(cat /proc/cmdline); do
            case $_x in ip=*) configure_networking; break ;; esac
        done
    fi

    # Native environment: modprobe automatically resolves all underlying dependencies.
    modprobe erofs 2>/dev/null || true
    modprobe overlay 2>/dev/null || true
    modprobe ext4 2>/dev/null || true

    for x in $(cat /proc/cmdline); do
        case $x in
            cocoon.layers=*) LAYERS="${x#cocoon.layers=}" ;;
            cocoon.cow=*)    COW="${x#cocoon.cow=}" ;;
            cocoon.timeout=*) COCOON_TIMEOUT="${x#cocoon.timeout=}" ;;
        esac
    done

    [ -z "$LAYERS" ] && panic "cocoon.layers= not set"
    [ -z "$COW" ]    && panic "cocoon.cow= not set"

    # Wait for udev to finish processing all pending events once, before any disk lookups.
    udevadm settle 2>/dev/null || true

    COCOON_INTERNAL="/.cocoon"
    mkdir -p "$COCOON_INTERNAL"

    # Mount read-only EROFS layers
    LOWER=""
    LAYER_DEVS=""
    IFS=,
    for serial in $LAYERS; do
        dev=$(resolve_disk "$serial") || panic "device ${serial} not found"
        mnt="${COCOON_INTERNAL}/layers/${serial}"
        mkdir -p "$mnt"
        mount -t erofs -o ro "$dev" "$mnt" || panic "mount ${serial} failed"
        [ -n "$LOWER" ] && LOWER="${LOWER}:"
        LOWER="${LOWER}${mnt}"
        LAYER_DEVS="${LAYER_DEVS} ${dev}"
    done
    unset IFS

    # Mount COW disk
    cow_dev=$(resolve_disk "$COW") || panic "COW device ${COW} not found"
    mkdir -p "${COCOON_INTERNAL}/cow"
    # [Performance] Added noatime to reduce unnecessary write operations on the COW disk.
    mount -t ext4 -o noatime "$cow_dev" "${COCOON_INTERNAL}/cow" || panic "mount COW failed"
    mkdir -p "${COCOON_INTERNAL}/cow/upper" "${COCOON_INTERNAL}/cow/work"

    # Assemble Overlayfs
    # [Optimized OverlayFS Options]
    # index=on: Prevents broken file handles and ensures inode consistency during copy-up.
    # redirect_dir=on: Enables renaming of directories that exist in the lower (read-only) layers.
    # metacopy=on: Optimizes metadata-only changes (like chmod/chown) to avoid full file copy-up.
    OVL_OPTS="lowerdir=${LOWER},upperdir=${COCOON_INTERNAL}/cow/upper,workdir=${COCOON_INTERNAL}/cow/work,index=on,redirect_dir=on,metacopy=on,xino=on"
    
    mount -t overlay overlay -o "$OVL_OPTS" "${rootmnt}" || panic "overlay failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"

    # [IO Performance Optimization]
    # EROFS layers are read-only and shared; "none" removes guest-side scheduling
    # overhead on the guest block device for pure-read lower layers.
    # COW disk gets mq-deadline to prevent write starvation under mixed read/write load.
    for dev in $LAYER_DEVS; do
        blk="${dev##*/}"
        [ -e "/sys/block/${blk}/queue/scheduler" ] && echo "none" > "/sys/block/${blk}/queue/scheduler" 2>/dev/null || true
    done
    cow_blk="${cow_dev##*/}"
    [ -e "/sys/block/${cow_blk}/queue/scheduler" ] && echo "mq-deadline" > "/sys/block/${cow_blk}/queue/scheduler" 2>/dev/null || true

    # Note: The systemd compatibility hacks (clearing fstab, masking fsck) 
    # are handled natively in the Dockerfile. The rootfs is clean here.

    # Binderfs: mount before switch_root so Android init finds /dev/binder* immediately.
    # This does not depend on udev/systemd. Android init will see /dev/binderfs/ and
    # the symlinks at /dev/binder, /dev/hwbinder, /dev/vndbinder.
    modprobe binder_linux 2>/dev/null || true
    mkdir -p "${rootmnt}/dev/binderfs"
    mount -t binder binder "${rootmnt}/dev/binderfs" 2>/dev/null || true
    for b in binder hwbinder vndbinder; do
        [ -e "${rootmnt}/dev/binderfs/${b}" ] && ln -sf "/dev/binderfs/${b}" "${rootmnt}/dev/${b}" 2>/dev/null || true
    done

    # Machine-ID isolation for cloned VMs.
    # Guard: Android rootfs has no /etc dir (uses /system/etc or absent).
    if [ -d "${rootmnt}/etc" ]; then
        rm -f "${rootmnt}/etc/machine-id" 2>/dev/null || true
        : > "${rootmnt}/etc/machine-id"
    fi

    log_success_msg "Cocoon: stealth overlay rootfs ready"
}
