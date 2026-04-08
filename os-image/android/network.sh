#!/system/bin/sh
# /system/bin/cocoon-network.sh
#
# Fix networking in cocoon VM where ConnectivityService doesn't manage eth0.
#
# Problem: Android netd adds "32000: from all unreachable" catch-all ip rule.
# netd monitors netlink and removes any ip rules it doesn't manage,
# so raw "ip rule add" gets silently deleted.
#
# Solution: program default routes inside netd-managed policy tables
# (legacy_system, legacy_network, local_network). This survives netd rule
# ownership and avoids hitting the 32000 unreachable fallback.

IFACE=eth0
TABLES="legacy_system legacy_network local_network"

cmdline_ip() {
    for x in $(cat /proc/cmdline); do
        case "$x" in
            ip=*) echo "${x#ip=}"; return 0 ;;
        esac
    done
    return 1
}

main_gw() {
    ip -4 route show table main 2>/dev/null \
        | sed -n 's/^default via \([0-9.]*\).*$/\1/p' \
        | head -n1
}

main_subnet() {
    ip -4 route show table main 2>/dev/null \
        | sed -n "s#^\([0-9.][0-9./]*\) dev ${IFACE} .*#\1#p" \
        | head -n1
}

iface_src() {
    ip -4 -o addr show dev "$IFACE" 2>/dev/null \
        | sed -n 's/.* inet \([0-9.]*\)\/.*/\1/p' \
        | head -n1
}

CMDLINE_IP="$(cmdline_ip || true)"
if [ -n "$CMDLINE_IP" ]; then
    CMDLINE_IFACE="$(printf '%s' "$CMDLINE_IP" | cut -d: -f6)"
    [ -n "$CMDLINE_IFACE" ] && IFACE="$CMDLINE_IFACE"
    CMDLINE_GW="$(printf '%s' "$CMDLINE_IP" | cut -d: -f3)"
    CMDLINE_DNS1="$(printf '%s' "$CMDLINE_IP" | cut -d: -f8)"
    CMDLINE_DNS2="$(printf '%s' "$CMDLINE_IP" | cut -d: -f9)"
fi

ip link set "$IFACE" up 2>/dev/null || true

# No kernel ip= — run DHCP via busybox udhcpc.
# This covers dhcp-noipam CNI networks where the guest must obtain its own IP.
# The udhcpc script writes the gateway to a temp file so we can read it after
# udhcpc exits — netd may delete routes from the main table before we get to
# the policy table sync below.
if [ -z "$CMDLINE_IP" ] && [ -x /sbin/busybox ]; then
    log -t cocoon-network "no ip= cmdline, running udhcpc on $IFACE"
    UDHCPC_SCRIPT="/tmp/udhcpc.sh"
    cat > "$UDHCPC_SCRIPT" << 'DHCPSCRIPT'
#!/bin/sh
case "$1" in
    bound|renew)
        ip addr flush dev "$interface" 2>/dev/null
        ip addr add "$ip/$mask" dev "$interface"
        [ -n "$router" ] && ip route replace default via "$router" dev "$interface" 2>/dev/null
        echo "$router" > /tmp/udhcpc_gw
        echo "$dns" > /tmp/udhcpc_dns
        ;;
esac
DHCPSCRIPT
    chmod 0755 "$UDHCPC_SCRIPT"
    /sbin/busybox udhcpc -i "$IFACE" -n -q -f -s "$UDHCPC_SCRIPT" 2>/dev/null
    # Read gateway/DNS saved by the udhcpc script before netd clears the route.
    [ -f /tmp/udhcpc_gw ] && CMDLINE_GW="$(cat /tmp/udhcpc_gw)"
    if [ -f /tmp/udhcpc_dns ]; then
        CMDLINE_DNS1="$(cat /tmp/udhcpc_dns | awk '{print $1}')"
        CMDLINE_DNS2="$(cat /tmp/udhcpc_dns | awk '{print $2}')"
    fi
fi

# Wait for netd to finish setting up its ip rules (policy tables + 32000 unreachable).
# Triggered by init.svc.netd=running, but netd needs a moment to initialize rules.
try=0
while [ $try -lt 10 ]; do
    ip rule show 2>/dev/null | grep -q 'unreachable' && break
    sleep 1
    try=$((try + 1))
done

# Discover gateway from main table first; fallback to ip= cmdline if needed.
GW=""
try=0
while [ $try -lt 10 ]; do
    GW="$(main_gw)"
    [ -n "$GW" ] && break
    if [ -n "$CMDLINE_GW" ]; then
        GW="$CMDLINE_GW"
        break
    fi
    sleep 1
    try=$((try + 1))
done

# Populate subnet route when available (helps policy tables resolve L2 quickly).
SUBNET="$(main_subnet)"
SRC="$(iface_src)"

# Program netd-managed policy tables.
if [ -z "$GW" ]; then
    log -t cocoon-network "WARN: no default gateway found (main/cmdline); skip policy route sync"
else
    for T in $TABLES; do
        if [ -n "$SUBNET" ] && [ -n "$SRC" ]; then
            ip route replace "$SUBNET" dev "$IFACE" src "$SRC" scope link table "$T" 2>/dev/null || true
        fi
        ip route replace default via "$GW" dev "$IFACE" onlink table "$T" 2>/dev/null || true
    done
    # Keep main route present as a fallback for tools that consult main directly.
    ip route replace default via "$GW" dev "$IFACE" onlink table main 2>/dev/null || true
fi

# Set DNS (no ConnectivityService to configure resolvers).
DNS1="${CMDLINE_DNS1:-8.8.8.8}"
DNS2="${CMDLINE_DNS2:-1.1.1.1}"
setprop net.dns1 "$DNS1"
setprop net.dns2 "$DNS2"

log -t cocoon-network "iface=$IFACE gw=${GW:-none} tables=[$TABLES] dns=[$DNS1,$DNS2]"
