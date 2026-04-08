#!/system/bin/sh
# /system/bin/cocoon-network.sh
#
# Fix networking in cocoon VM where ConnectivityService doesn't manage eth0.
#
# Problem: Android netd adds "32000: from all unreachable" catch-all ip rule.
# Solution: copy default routes into netd-managed policy tables so traffic
# doesn't hit the 32000 unreachable fallback.
#
# Requires kernel ip= parameter (static IP from CNI). DHCP networks are not
# supported for Android VMs — netd blocks external route modifications after
# boot, and ipconfigstore cannot read the gateway without a pre-existing route.

IFACE=eth0
TABLES="legacy_system legacy_network local_network"

# Guard against repeated invocations — cocoon-network.rc triggers on every
# netd start/restart. Only run once.
GUARD="/data/local/tmp/.cocoon-network-done"
[ -f "$GUARD" ] && exit 0

cmdline_ip() {
    local cmdline
    cmdline="$(cat /proc/cmdline 2>/dev/null)" || \
    cmdline="$(tr '\0' ' ' < /proc/1/cmdline 2>/dev/null)" || \
    return 1
    for x in $cmdline; do
        case "$x" in
            ip=*) echo "${x#ip=}"; return 0 ;;
        esac
    done
    return 1
}

CMDLINE_IP="$(cmdline_ip || true)"
CMDLINE_GW="" CMDLINE_DNS1="" CMDLINE_DNS2=""
if [ -n "$CMDLINE_IP" ]; then
    CMDLINE_IFACE="$(printf '%s' "$CMDLINE_IP" | cut -d: -f6)"
    [ -n "$CMDLINE_IFACE" ] && IFACE="$CMDLINE_IFACE"
    CMDLINE_GW="$(printf '%s' "$CMDLINE_IP" | cut -d: -f3)"
    CMDLINE_DNS1="$(printf '%s' "$CMDLINE_IP" | cut -d: -f8)"
    CMDLINE_DNS2="$(printf '%s' "$CMDLINE_IP" | cut -d: -f9)"
fi

ip link set "$IFACE" up 2>/dev/null || true

# Wait for netd to finish initializing ip rules.
try=0
while [ $try -lt 10 ]; do
    ip rule show 2>/dev/null | grep -q 'unreachable' && break
    sleep 1
    try=$((try + 1))
done

# Discover gateway: main table first, then kernel cmdline.
GW=""
try=0
while [ $try -lt 10 ]; do
    GW="$(ip -4 route show table main 2>/dev/null | sed -n 's/^default via \([0-9.]*\).*$/\1/p' | head -1)"
    [ -n "$GW" ] && break
    [ -n "$CMDLINE_GW" ] && GW="$CMDLINE_GW" && break
    sleep 1
    try=$((try + 1))
done

# Copy routes into netd policy tables.
if [ -z "$GW" ]; then
    log -t cocoon-network "WARN: no gateway found; skip route setup"
else
    SUBNET="$(ip -4 route show table main 2>/dev/null | sed -n "s#^\([0-9.][0-9./]*\) dev ${IFACE} .*#\1#p" | head -1)"
    SRC="$(ip -4 -o addr show dev "$IFACE" 2>/dev/null | sed -n 's/.* inet \([0-9.]*\)\/.*/\1/p' | head -1)"
    for T in $TABLES; do
        [ -n "$SUBNET" ] && [ -n "$SRC" ] && \
            ip route replace "$SUBNET" dev "$IFACE" src "$SRC" scope link table "$T" 2>/dev/null || true
        ip route replace default via "$GW" dev "$IFACE" onlink table "$T" 2>/dev/null || true
    done
    ip route replace default via "$GW" dev "$IFACE" onlink table main 2>/dev/null || true
    touch "$GUARD"
fi

# Set DNS.
DNS1="${CMDLINE_DNS1:-8.8.8.8}"
DNS2="${CMDLINE_DNS2:-1.1.1.1}"
setprop net.dns1 "$DNS1"
setprop net.dns2 "$DNS2"

log -t cocoon-network "iface=$IFACE gw=${GW:-none} dns=[$DNS1,$DNS2]"
