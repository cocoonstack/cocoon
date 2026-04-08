#!/system/bin/sh
# /system/bin/cocoon-network.sh
#
# Fix networking in cocoon VM where ConnectivityService doesn't manage eth0.
#
# Static IP: kernel ip= configures routes before netd. ipconfigstore writes
# ipconfig.txt with STATIC config. EthernetService picks it up automatically.
# This script copies routes into netd policy tables as a safety net.
#
# DHCP: no kernel ip=. Delete ipconfig.txt (which ipconfigstore wrote with
# empty IP) so EthernetService falls back to its default DHCP mode. Android's
# built-in DhcpClient handles everything through ConnectivityService/netd.

IFACE=eth0
TABLES="legacy_system legacy_network local_network"

# Guard against repeated invocations.
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

ip link set "$IFACE" up 2>/dev/null || true

# Wait for netd to finish initializing ip rules.
try=0
while [ $try -lt 10 ]; do
    ip rule show 2>/dev/null | grep -q 'unreachable' && break
    sleep 1
    try=$((try + 1))
done

if [ -n "$CMDLINE_IP" ]; then
    # Static IP: kernel ip= already configured main table.
    CMDLINE_GW="$(printf '%s' "$CMDLINE_IP" | cut -d: -f3)"
    CMDLINE_DNS1="$(printf '%s' "$CMDLINE_IP" | cut -d: -f8)"
    CMDLINE_DNS2="$(printf '%s' "$CMDLINE_IP" | cut -d: -f9)"

    GW=""
    try=0
    while [ $try -lt 10 ]; do
        GW="$(ip -4 route show table main 2>/dev/null | sed -n 's/^default via \([0-9.]*\).*$/\1/p' | head -1)"
        [ -n "$GW" ] && break
        [ -n "$CMDLINE_GW" ] && GW="$CMDLINE_GW" && break
        sleep 1
        try=$((try + 1))
    done

    if [ -n "$GW" ]; then
        SUBNET="$(ip -4 route show table main 2>/dev/null | sed -n "s#^\([0-9.][0-9./]*\) dev ${IFACE} .*#\1#p" | head -1)"
        SRC="$(ip -4 -o addr show dev "$IFACE" 2>/dev/null | sed -n 's/.* inet \([0-9.]*\)\/.*/\1/p' | head -1)"
        for T in $TABLES; do
            [ -n "$SUBNET" ] && [ -n "$SRC" ] && \
                ip route replace "$SUBNET" dev "$IFACE" src "$SRC" scope link table "$T" 2>/dev/null || true
            ip route replace default via "$GW" dev "$IFACE" onlink table "$T" 2>/dev/null || true
        done
        ip route replace default via "$GW" dev "$IFACE" onlink table main 2>/dev/null || true
    fi

    DNS1="${CMDLINE_DNS1:-8.8.8.8}"
    DNS2="${CMDLINE_DNS2:-1.1.1.1}"
    setprop net.dns1 "$DNS1"
    setprop net.dns2 "$DNS2"
    log -t cocoon-network "static: iface=$IFACE gw=${GW:-none} dns=[$DNS1,$DNS2]"
else
    # DHCP: delete ipconfig.txt so EthernetService uses default DHCP mode.
    # ipconfigstore already ran at post-fs-data and wrote a broken STATIC config
    # (empty IP, 0.0.0.0 gateway). Remove it and bounce the interface so
    # EthernetTracker re-provisions with DHCP via Android's built-in DhcpClient.
    rm -f /data/misc/ethernet/ipconfig.txt 2>/dev/null
    rm -f /data/misc/apexdata/com.android.tethering/misc/ethernet/ipconfig.txt 2>/dev/null
    ip link set "$IFACE" down 2>/dev/null
    sleep 1
    ip link set "$IFACE" up 2>/dev/null
    log -t cocoon-network "dhcp: deleted ipconfig.txt, bounced $IFACE for EthernetService DHCP"
fi

touch "$GUARD"
