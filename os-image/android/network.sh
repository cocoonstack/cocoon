#!/system/bin/sh
# /system/bin/cocoon-network.sh
#
# Fix networking in cocoon VM where ConnectivityService doesn't manage eth0.
#
# Problem: Android netd adds "32000: from all unreachable" catch-all ip rule
# and controls all policy routing tables. Direct "ip route" writes are either
# rejected or silently cleaned up by netd's netlink monitor.
#
# Solution: use ndc (netd command interface) to register the network, add the
# interface, set up routes, and mark it as default. This makes netd own the
# routes and populate all policy tables correctly.

IFACE=eth0
NETID=100

# Guard against repeated invocations — cocoon-network.rc triggers on every
# netd start/restart. Only run once; subsequent calls are no-ops.
GUARD="/tmp/.cocoon-network-done"
if [ -f "$GUARD" ]; then
    exit 0
fi

cmdline_ip() {
    for x in $(cat /proc/cmdline); do
        case "$x" in
            ip=*) echo "${x#ip=}"; return 0 ;;
        esac
    done
    return 1
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
    [ -f /tmp/udhcpc_gw ] && CMDLINE_GW="$(cat /tmp/udhcpc_gw)"
    if [ -f /tmp/udhcpc_dns ]; then
        CMDLINE_DNS1="$(cat /tmp/udhcpc_dns | awk '{print $1}')"
        CMDLINE_DNS2="$(cat /tmp/udhcpc_dns | awk '{print $2}')"
    fi
fi

# Wait for netd to finish initializing ip rules.
try=0
while [ $try -lt 10 ]; do
    ip rule show 2>/dev/null | grep -q 'unreachable' && break
    sleep 1
    try=$((try + 1))
done

# Discover gateway: main table first, then kernel cmdline / udhcpc result.
GW=""
try=0
while [ $try -lt 10 ]; do
    GW="$(ip -4 route show table main 2>/dev/null | sed -n 's/^default via \([0-9.]*\).*$/\1/p' | head -1)"
    [ -n "$GW" ] && break
    if [ -n "$CMDLINE_GW" ]; then
        GW="$CMDLINE_GW"
        break
    fi
    sleep 1
    try=$((try + 1))
done

if [ -z "$GW" ]; then
    log -t cocoon-network "WARN: no gateway found; skip ndc setup"
else
    # Register network with netd so it owns the routes and policy tables.
    # Destroy first in case a previous run left a stale network registration.
    ndc network destroy "$NETID" 2>/dev/null
    ndc network create "$NETID" 2>/dev/null
    ndc network interface add "$NETID" "$IFACE" 2>/dev/null
    SUBNET="$(ip -4 route show table main 2>/dev/null | sed -n "s#^\([0-9.][0-9./]*\) dev ${IFACE} .*#\1#p" | head -1)"
    [ -n "$SUBNET" ] && ndc network route add "$NETID" "$IFACE" "$SUBNET" 2>/dev/null
    ndc network route add "$NETID" "$IFACE" 0.0.0.0/0 "$GW" 2>/dev/null
    ndc network default set "$NETID" 2>/dev/null
    ndc network permission user set NETWORK "$NETID" 2>/dev/null
    touch "$GUARD"
fi

# Set DNS (no ConnectivityService to configure resolvers).
DNS1="${CMDLINE_DNS1:-8.8.8.8}"
DNS2="${CMDLINE_DNS2:-1.1.1.1}"
setprop net.dns1 "$DNS1"
setprop net.dns2 "$DNS2"

log -t cocoon-network "iface=$IFACE gw=${GW:-none} netid=$NETID dns=[$DNS1,$DNS2]"
