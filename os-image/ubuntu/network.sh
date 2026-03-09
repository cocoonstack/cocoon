#!/bin/sh
# Filename: network.sh
# Target path: /etc/initramfs-tools/scripts/init-bottom/cocoon-network
#
# Runs in init-bottom phase — AFTER configure_networking has parsed kernel ip=
# parameters into /run/net-*.conf, and AFTER mountroot has assembled the overlay.
# Converts initramfs network config into systemd-networkd .network files so
# the IP configuration persists after switch_root, and writes /etc/resolv.conf
# for immediate DNS availability regardless of init system.

PREREQ=""
prereqs() { echo "$PREREQ"; }
case "$1" in prereqs) prereqs; exit 0 ;; esac

. /scripts/functions

# $rootmnt is set by initramfs — points to the mounted root filesystem.
[ -z "$rootmnt" ] && exit 0

_dns_servers=""
_has_static=false

for conf_file in /run/net-*.conf; do
    [ -f "$conf_file" ] || continue

    unset DEVICE IPV4ADDR IPV4NETMASK IPV4GATEWAY IPV4DNS0 IPV4DNS1 HOSTNAME HWADDR
    . "$conf_file"
    [ -z "$DEVICE" ] && continue

    # Set hostname even if no IP (DHCP mode with ip=::::hostname:dev:off).
    if [ -n "$HOSTNAME" ] && [ ! -f "${rootmnt}/etc/cocoon-hostname-set" ]; then
        echo "$HOSTNAME" > "${rootmnt}/etc/hostname"
        : > "${rootmnt}/etc/cocoon-hostname-set"
    fi

    [ -z "$IPV4ADDR" ] && continue

    # Read MAC from sysfs if HWADDR not in conf (older klibc).
    [ -z "$HWADDR" ] && [ -e "/sys/class/net/${DEVICE}/address" ] && HWADDR=$(cat "/sys/class/net/${DEVICE}/address")
    [ -z "$HWADDR" ] && continue

    _has_static=true

    # Convert dotted netmask to prefix length.
    prefix=0
    IFS=. read -r a b c d <<EOF
${IPV4NETMASK}
EOF
    for octet in $a $b $c $d; do
        case $octet in
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

    # Use MAC-based matching so the config works regardless of device naming
    # (eth0, enp0s4, or any name after hot-swap). File name uses MAC without
    # colons to avoid collisions with old device-name-based files.
    mac_sanitized=$(echo "$HWADDR" | tr -d ':')
    mkdir -p "${rootmnt}/etc/systemd/network"
    {
        printf "[Match]\nMACAddress=%s\n\n[Network]\nAddress=%s/%d\n" "$HWADDR" "$IPV4ADDR" "$prefix"
        [ -n "$IPV4GATEWAY" ] && [ "$IPV4GATEWAY" != "0.0.0.0" ] && printf "Gateway=%s\n" "$IPV4GATEWAY"
        [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ] && printf "DNS=%s\n" "$IPV4DNS0"
        [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ] && printf "DNS=%s\n" "$IPV4DNS1"
        # Fallback DNS if none provided.
        if [ -z "$IPV4DNS0" ] || [ "$IPV4DNS0" = "0.0.0.0" ]; then
            printf "DNS=8.8.8.8\nDNS=8.8.4.4\n"
        fi
    } > "${rootmnt}/etc/systemd/network/10-${mac_sanitized}.network"

    # Collect DNS servers for resolv.conf.
    [ -n "$IPV4DNS0" ] && [ "$IPV4DNS0" != "0.0.0.0" ] && _dns_servers="${_dns_servers} ${IPV4DNS0}"
    [ -n "$IPV4DNS1" ] && [ "$IPV4DNS1" != "0.0.0.0" ] && _dns_servers="${_dns_servers} ${IPV4DNS1}"

done

# Fallback: no kernel ip= configured — write DHCP config per NIC matched by MAC.
# This covers macvlan / external DHCP scenarios where CNI does not assign IPs.
if [ "$_has_static" = false ]; then
    mkdir -p "${rootmnt}/etc/systemd/network"
    for sysdev in /sys/class/net/*; do
        [ -e "$sysdev" ] || continue
        dev=$(basename "$sysdev")
        # Skip loopback and virtual devices.
        case "$dev" in lo|bonding_masters) continue ;; esac
        [ -e "${sysdev}/address" ] || continue
        mac=$(cat "${sysdev}/address")
        # Skip zero/empty MACs.
        case "$mac" in ""|00:00:00:00:00:00) continue ;; esac
        mac_sanitized=$(echo "$mac" | tr -d ':')
        {
            printf "[Match]\nMACAddress=%s\n\n[Network]\nDHCP=ipv4\n" "$mac"
        } > "${rootmnt}/etc/systemd/network/10-${mac_sanitized}.network"
    done
fi

# Write /etc/resolv.conf from DNS servers collected above.
[ -z "$_dns_servers" ] && _dns_servers="8.8.8.8 8.8.4.4"
: > "${rootmnt}/etc/resolv.conf"
for _ns in $_dns_servers; do
    printf "nameserver %s\n" "$_ns" >> "${rootmnt}/etc/resolv.conf"
done
