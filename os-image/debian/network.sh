#!/bin/sh
# Vendored from cocoon/os-image/ubuntu/network.sh at Cocoon v0.5.9
# (144927060c3e90dbe2f3e1a15143572c402958de), with Debian 13
# systemd-resolved symlink handling.
# Target path: /etc/initramfs-tools/scripts/init-bottom/cocoon-network

PREREQ=""
prereqs() { printf '%s\n' "$PREREQ"; }
case "${1:-}" in
    prereqs) prereqs; exit 0 ;;
esac

# Supplied by initramfs-tools at boot.
# shellcheck source=/dev/null
. /scripts/functions

# rootmnt is supplied by initramfs-tools and points at the overlay root.
# shellcheck disable=SC2154
[ -n "$rootmnt" ] || exit 0

strip_colons() {
    value=$1
    while [ "${value#*:}" != "$value" ]; do
        value="${value%%:*}${value#*:}"
    done
    printf '%s\n' "$value"
}

cmdline=$(cat /proc/cmdline)
# Kernel command lines are space-delimited by definition.
# shellcheck disable=SC2086
set -- $cmdline
for arg do
    case "$arg" in
        cocoon.hostname=*) printf '%s\n' "${arg#cocoon.hostname=}" > "${rootmnt}/etc/hostname" ;;
    esac
done

dns_servers=""
has_static=false

for conf_file in /run/net-*.conf; do
    [ -f "$conf_file" ] || continue

    unset DEVICE IPV4ADDR IPV4NETMASK IPV4GATEWAY IPV4DNS0 IPV4DNS1 HOSTNAME HWADDR
    # The file is generated and parsed by initramfs-tools' ipconfig support.
    # shellcheck source=/dev/null
    . "$conf_file"
    [ -n "$DEVICE" ] || continue
    [ -n "$IPV4ADDR" ] || continue

    # Older klibc output can omit HWADDR.
    if [ -z "$HWADDR" ] && [ -e "/sys/class/net/${DEVICE}/address" ]; then
        HWADDR=$(cat "/sys/class/net/${DEVICE}/address")
    fi
    [ -n "$HWADDR" ] || continue

    has_static=true

    # Convert the dotted netmask produced by ipconfig to a prefix length.
    prefix=0
    IFS=. read -r octet_a octet_b octet_c octet_d <<EOF
$IPV4NETMASK
EOF
    for octet in "$octet_a" "$octet_b" "$octet_c" "$octet_d"; do
        case "$octet" in
            255) prefix=$((prefix + 8)) ;;
            254) prefix=$((prefix + 7)) ;;
            252) prefix=$((prefix + 6)) ;;
            248) prefix=$((prefix + 5)) ;;
            240) prefix=$((prefix + 4)) ;;
            224) prefix=$((prefix + 3)) ;;
            192) prefix=$((prefix + 2)) ;;
            128) prefix=$((prefix + 1)) ;;
        esac
    done

    # MAC matching survives interface renaming and VM cloning.
    # initramfs-tools does not include coreutils' tr by default.
    mac_sanitized=$(strip_colons "$HWADDR")
    mkdir -p "${rootmnt}/etc/systemd/network"
    {
        printf '[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n' "$HWADDR" "$IPV4ADDR" "$prefix"
        [ -n "$IPV4GATEWAY" ] && [ "$IPV4GATEWAY" != "0.0.0.0" ] && printf 'Gateway=%s\n' "$IPV4GATEWAY"
        [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ] && printf 'DNS=%s\n' "$IPV4DNS0"
        [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ] && printf 'DNS=%s\n' "$IPV4DNS1"
        if [ -z "$IPV4DNS0" ] || [ "$IPV4DNS0" = "0.0.0.0" ]; then
            printf 'DNS=8.8.8.8\nDNS=8.8.4.4\n'
        fi
    } > "${rootmnt}/etc/systemd/network/10-${mac_sanitized}.network"

    [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ] && dns_servers="${dns_servers} ${IPV4DNS0}"
    [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ] && dns_servers="${dns_servers} ${IPV4DNS1}"
done

# Without kernel-provided static data, persist clone-safe DHCP per physical NIC.
if [ "$has_static" = false ]; then
    mkdir -p "${rootmnt}/etc/systemd/network"
    for sysdev in /sys/class/net/*; do
        [ -e "$sysdev" ] || continue
        dev="${sysdev##*/}"
        case "$dev" in
            lo|bonding_masters) continue ;;
        esac
        [ -e "${sysdev}/address" ] || continue
        mac=$(cat "${sysdev}/address")
        case "$mac" in
            ''|00:00:00:00:00:00) continue ;;
        esac
        mac_sanitized=$(strip_colons "$mac")
        {
            printf '[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n\n[DHCPv4]\nClientIdentifier=mac\n' "$mac"
        } > "${rootmnt}/etc/systemd/network/10-${mac_sanitized}.network"
    done
fi

[ -n "$dns_servers" ] || dns_servers="8.8.8.8 8.8.4.4"

# Debian's systemd-resolved package creates this relative symlink at install
# time. Its /run target is normally absent in the initramfs, so create the
# target directory and write the target directly. Never follow an unexpected
# symlink outside rootmnt.
resolv_conf="${rootmnt}/etc/resolv.conf"
resolv_output="$resolv_conf"
if [ -L "$resolv_conf" ]; then
    resolv_target=$(readlink "$resolv_conf")
    case "$resolv_target" in
        ../run/systemd/resolve/stub-resolv.conf)
            mkdir -p "${rootmnt}/run/systemd/resolve"
            resolv_output="${rootmnt}/run/systemd/resolve/stub-resolv.conf"
            ;;
        *)
            printf 'cocoon-network: replacing unsupported resolv.conf symlink: %s\n' "$resolv_target" >&2
            rm -f "$resolv_conf"
            ;;
    esac
fi

{
    for nameserver in $dns_servers; do
        printf 'nameserver %s\n' "$nameserver"
    done
} > "$resolv_output"
