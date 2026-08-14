#!/bin/sh
# Vendored from cocoon/os-image/ubuntu/overlay.sh at Cocoon v0.5.9
# (144927060c3e90dbe2f3e1a15143572c402958de).
# Target path: /etc/initramfs-tools/scripts/cocoon-overlay

# Supplied by initramfs-tools at boot.
# shellcheck source=/dev/null
. /scripts/functions

resolve_disk() {
    serial="$1"
    timeout="${COCOON_TIMEOUT:-10}"
    i=0
    case "$timeout" in
        ''|*[!0-9]*) timeout=10 ;;
    esac

    # Firecracker exposes direct /dev/vdX paths and no virtio serial.
    case "$serial" in
        /dev/*)
            while [ "$i" -lt "$timeout" ]; do
                [ -b "$serial" ] && printf '%s\n' "$serial" && return 0
                sleep 1
                i=$((i + 1))
            done
            return 1
            ;;
    esac

    # Cloud Hypervisor exposes virtio-blk serials.
    while [ "$i" -lt "$timeout" ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            disk_serial=""
            [ -f "$sysdev/serial" ] && disk_serial=$(cat "$sysdev/serial")
            [ -f "$sysdev/device/serial" ] && disk_serial=$(cat "$sysdev/device/serial")

            # Trim trailing whitespace.
            while :; do
                case "$disk_serial" in
                    *[[:space:]]) disk_serial="${disk_serial%[[:space:]]}" ;;
                    *) break ;;
                esac
            done

            if [ "$disk_serial" = "$serial" ]; then
                printf '/dev/%s\n' "${sysdev##*/}"
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

    # Only ask initramfs-tools to configure networking when an ip= argument is
    # present. Probing without one delays no-NIC guests substantially.
    cmdline=$(cat /proc/cmdline)
    case " $cmdline " in
        *' ip='*)
            if ! ls /run/net-*.conf >/dev/null 2>&1; then
                configure_networking
            fi
            ;;
    esac

    # modprobe resolves the dependency closure for modular features. Built-in
    # features legitimately make modprobe fail and need no initramfs object.
    modprobe erofs 2>/dev/null || true
    modprobe overlay 2>/dev/null || true
    modprobe ext4 2>/dev/null || true

    LAYERS=""
    COW=""
    # Kernel command lines are space-delimited by definition.
    # shellcheck disable=SC2086
    set -- $cmdline
    for arg do
        case "$arg" in
            cocoon.layers=*) LAYERS="${arg#cocoon.layers=}" ;;
            cocoon.cow=*) COW="${arg#cocoon.cow=}" ;;
            cocoon.timeout=*) COCOON_TIMEOUT="${arg#cocoon.timeout=}" ;;
        esac
    done

    [ -n "$LAYERS" ] || panic "cocoon.layers= not set"
    [ -n "$COW" ] || panic "cocoon.cow= not set"

    udevadm settle 2>/dev/null || true

    COCOON_INTERNAL="/.cocoon"
    mkdir -p "$COCOON_INTERNAL"

    # Mount immutable EROFS layers in the order supplied by Cocoon.
    LOWER=""
    LAYER_DEVS=""
    LAYER_MOUNTS=""
    old_ifs=$IFS
    IFS=,
    for serial in $LAYERS; do
        dev=$(resolve_disk "$serial") || panic "device ${serial} not found"
        mnt="${COCOON_INTERNAL}/layers/${serial}"
        mkdir -p "$mnt"
        mount -t erofs -o ro "$dev" "$mnt" || panic "mount ${serial} failed"
        [ -n "$LOWER" ] && LOWER="${LOWER}:"
        LOWER="${LOWER}${mnt}"
        LAYER_DEVS="${LAYER_DEVS} ${dev}"
        LAYER_MOUNTS="${LAYER_MOUNTS} ${mnt}"
    done
    IFS=$old_ifs

    # Mount the per-VM ext4 COW disk.
    cow_dev=$(resolve_disk "$COW") || panic "COW device ${COW} not found"
    mkdir -p "${COCOON_INTERNAL}/cow"
    mount -t ext4 -o noatime "$cow_dev" "${COCOON_INTERNAL}/cow" || panic "mount COW failed"
    mkdir -p "${COCOON_INTERNAL}/cow/upper" "${COCOON_INTERNAL}/cow/work"

    OVL_OPTS="lowerdir=${LOWER},upperdir=${COCOON_INTERNAL}/cow/upper,workdir=${COCOON_INTERNAL}/cow/work,index=on,redirect_dir=on,metacopy=on,xino=on"
    # Debian's initramfs uses klibc mount, which requires all options before
    # the device and directory operands. rootmnt is supplied by initramfs-tools.
    # shellcheck disable=SC2154
    mount -t overlay -o "$OVL_OPTS" overlay "$rootmnt" || panic "overlay failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"

    # run-init deletes the old initramfs before moving rootmnt onto /. It
    # deliberately skips mounted filesystems, then fails with ENOTEMPTY if any
    # backing mount remains below the initramfs root. Keep the backing mounts
    # alive by moving them below the new overlay root before switch-root.
    for mnt in $LAYER_MOUNTS; do
        mkdir -p "${rootmnt}${mnt}"
        mount -n -o move "$mnt" "${rootmnt}${mnt}" || panic "move ${mnt} into rootfs failed"
    done
    mkdir -p "${rootmnt}${COCOON_INTERNAL}/cow"
    mount -n -o move "${COCOON_INTERNAL}/cow" "${rootmnt}${COCOON_INTERNAL}/cow" \
        || panic "move COW mount into rootfs failed"

    # Avoid guest scheduler overhead for immutable layers; favor bounded COW
    # write latency under mixed workloads.
    for dev in $LAYER_DEVS; do
        block_name="${dev##*/}"
        if [ -e "/sys/block/${block_name}/queue/scheduler" ]; then
            printf 'none\n' > "/sys/block/${block_name}/queue/scheduler" 2>/dev/null || true
        fi
    done
    cow_block_name="${cow_dev##*/}"
    if [ -e "/sys/block/${cow_block_name}/queue/scheduler" ]; then
        printf 'mq-deadline\n' > "/sys/block/${cow_block_name}/queue/scheduler" 2>/dev/null || true
    fi

    # Every clone must get its own machine identity on first systemd boot.
    rm -f "${rootmnt}/etc/machine-id" 2>/dev/null || true
    : > "${rootmnt}/etc/machine-id"

    log_success_msg "Cocoon: stealth overlay rootfs ready"
}
