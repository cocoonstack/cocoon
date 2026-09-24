#!/usr/bin/env bash
# doctor/vm-inspect.sh — Deep diagnostic for a single Cocoon VM.
#
# Shows: PTY/console path, CH config (disks, nets, balloon, CPU, memory),
# per-NIC chain (veth ↔ tap, MAC, IP, TC filters, queue settings), and
# validates that tap queue count matches the CH --net num_queues.
#
# Usage:
#   ./doctor/vm-inspect.sh <VM_ID_or_NAME>
#
# Requirements: cocoon, jq, ip, tc, nsenter (standard on any Linux with iproute2).

set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
COCOON_ROOT_DIR="${COCOON_ROOT_DIR:-/var/lib/cocoon}"
COCOON_RUN_DIR="${COCOON_RUN_DIR:-/var/lib/cocoon/run}"

COCOON_BIN="${COCOON_BIN:-cocoon}"
CH_RUN_DIR="${COCOON_RUN_DIR}/cloudhypervisor"
FC_RUN_DIR="${COCOON_RUN_DIR}/firecracker"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; CYAN='\033[36m'; BOLD='\033[1m'; RESET='\033[0m'

pass()   { printf "  ${GREEN}[PASS]${RESET} %s\n" "$*"; }
fail()   { printf "  ${RED}[FAIL]${RESET} %s\n" "$*"; }
warn()   { printf "  ${YELLOW}[WARN]${RESET} %s\n" "$*"; }
info()   { printf "  ${CYAN}[INFO]${RESET} %s\n" "$*"; }
header() { printf "\n${BOLD}==> %s${RESET}\n" "$*"; }
kv()     { printf "  %-24s %s\n" "$1:" "$2"; }

die() { echo "error: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Args
# ---------------------------------------------------------------------------
if [ $# -lt 1 ] || [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    cat <<'EOF'
Usage: vm-inspect.sh <VM_ID_or_NAME>

Deep diagnostic for a Cocoon VM. Shows console/PTY path, full CH config,
per-NIC network chain (veth ↔ tap, MAC, IP, TC filters, queue counts),
and validates configuration consistency.

Environment:
  COCOON_BIN        (default: cocoon)
  COCOON_RUN_DIR    (default: /var/lib/cocoon/run)
EOF
    exit 0
fi

REF="$1"

# ---------------------------------------------------------------------------
# Dependency check
# ---------------------------------------------------------------------------
for cmd in jq ip tc curl "$COCOON_BIN"; do
    command -v "$cmd" &>/dev/null || die "$cmd not found in PATH"
done

# ---------------------------------------------------------------------------
# 1. VM record
# ---------------------------------------------------------------------------
header "VM Record"

VM_REC=$("$COCOON_BIN" vm inspect "$REF" 2>/dev/null) || die "VM '$REF' not found ($COCOON_BIN vm inspect failed)"
VM_ID=$(echo "$VM_REC" | jq -r '.id')
VM_NAME=$(echo "$VM_REC" | jq -r '.config.name // "<unnamed>"')
VM_NETWORK=$(echo "$VM_REC" | jq -r '.config.network // empty')
VM_STATE=$(echo "$VM_REC" | jq -r '.state // "unknown"')
VM_PID=$(echo "$VM_REC" | jq -r '.pid // 0')
VM_STALE=$(echo "$VM_REC" | jq -r '.stale // false')
CONSOLE_PATH=$(echo "$VM_REC" | jq -r '.console_path // empty')
case "$(echo "$VM_REC" | jq -r '.hypervisor // empty')" in
    firecracker) RUN_DIR="${FC_RUN_DIR}/${VM_ID}" ;;
    *)           RUN_DIR="${CH_RUN_DIR}/${VM_ID}" ;;
esac
SOCK_PATH=$(echo "$VM_REC" | jq -r ".socket_path // \"${RUN_DIR}/api.sock\"")

kv "ID" "$VM_ID"
kv "Name" "$VM_NAME"
kv "Network" "${VM_NETWORK:-<default>}"
kv "State" "$VM_STATE"
kv "PID" "$VM_PID"
kv "RunDir" "$RUN_DIR"

# ---------------------------------------------------------------------------
# 2. Process liveness
# ---------------------------------------------------------------------------
header "Process"

if [ "$VM_PID" -gt 0 ] 2>/dev/null && kill -0 "$VM_PID" 2>/dev/null; then
    PROC_EXE=$(readlink -f "/proc/$VM_PID/exe" 2>/dev/null || echo "unknown")
    pass "PID $VM_PID alive ($PROC_EXE)"
elif [ "$VM_STALE" = "true" ]; then
    fail "VMM gone but the record says 'running' (stale record)"
else
    info "PID $VM_PID not alive (state: $VM_STATE)"
fi

# ---------------------------------------------------------------------------
# 3. Console / PTY
# ---------------------------------------------------------------------------
header "Console / PTY"

if [ -n "$CONSOLE_PATH" ]; then
    kv "Console" "$CONSOLE_PATH"
    if [ -e "$CONSOLE_PATH" ]; then
        pass "Console endpoint exists"
    elif [ "$VM_STATE" = "running" ]; then
        fail "Console endpoint missing but VM is running"
    else
        info "Console endpoint absent (VM not running)"
    fi
else
    info "no console path recorded (VM not running)"
fi

# ---------------------------------------------------------------------------
# 4. CH Configuration
# ---------------------------------------------------------------------------
header "Cloud Hypervisor Config"

# Source priority: vm.info API (running VM) > config.json (clone) > DB record.
CH_CONFIG="${RUN_DIR}/config.json"
CH_CFG=""
CFG_SOURCE=""

if [ -S "$SOCK_PATH" ]; then
    CH_CFG=$(curl -s --unix-socket "$SOCK_PATH" http://localhost/api/v1/vm.info 2>/dev/null | jq '.config // empty' 2>/dev/null || echo "")
    [ -n "$CH_CFG" ] && [ "$CH_CFG" != "null" ] && CFG_SOURCE="vm.info API"
fi
if [ -z "$CFG_SOURCE" ] && [ -f "$CH_CONFIG" ]; then
    CH_CFG=$(jq '.' "$CH_CONFIG" 2>/dev/null || echo "")
    [ -n "$CH_CFG" ] && CFG_SOURCE="config.json (clone)"
fi

if [ -n "$CFG_SOURCE" ]; then
    info "source: $CFG_SOURCE"

    # CPU
    BOOT_CPUS=$(echo "$CH_CFG" | jq '.cpus.boot_vcpus // 0')
    MAX_CPUS=$(echo "$CH_CFG" | jq '.cpus.max_vcpus // 0')
    kv "CPUs" "boot=$BOOT_CPUS max=$MAX_CPUS"

    # Memory
    MEM_SIZE=$(echo "$CH_CFG" | jq '.memory.size // 0')
    MEM_HUGE=$(echo "$CH_CFG" | jq '.memory.hugepages // false')
    MEM_MB=$((MEM_SIZE / 1048576))
    kv "Memory" "${MEM_MB} MiB (hugepages=$MEM_HUGE)"

    # Balloon
    BALLOON=$(echo "$CH_CFG" | jq '.balloon // null')
    if [ "$BALLOON" != "null" ]; then
        BAL_SIZE=$(echo "$BALLOON" | jq '.size // 0')
        BAL_MB=$((BAL_SIZE / 1048576))
        BAL_OOM=$(echo "$BALLOON" | jq '.deflate_on_oom // false')
        BAL_FPR=$(echo "$BALLOON" | jq '.free_page_reporting // false')
        kv "Balloon" "${BAL_MB} MiB (deflate_on_oom=$BAL_OOM, free_page_reporting=$BAL_FPR)"
    else
        kv "Balloon" "disabled"
    fi

    # Watchdog
    WATCHDOG=$(echo "$CH_CFG" | jq '.watchdog // false')
    kv "Watchdog" "$WATCHDOG"

    # RNG
    RNG_SRC=$(echo "$CH_CFG" | jq -r '.rng.src // "none"')
    kv "RNG" "$RNG_SRC"

    # Payload
    PAYLOAD=$(echo "$CH_CFG" | jq '.payload // null')
    if [ "$PAYLOAD" != "null" ]; then
        PL_KERNEL=$(echo "$PAYLOAD" | jq -r '.kernel // empty')
        PL_INITRD=$(echo "$PAYLOAD" | jq -r '.initramfs // empty')
        PL_FW=$(echo "$PAYLOAD" | jq -r '.firmware // empty')
        [ -n "$PL_KERNEL" ] && kv "Kernel" "$PL_KERNEL"
        [ -n "$PL_INITRD" ] && kv "Initrd" "$PL_INITRD"
        [ -n "$PL_FW" ]     && kv "Firmware" "$PL_FW"
    fi

    # Disks
    DISK_COUNT=$(echo "$CH_CFG" | jq '.disks | length // 0')
    if [ "$DISK_COUNT" -gt 0 ]; then
        printf "\n  ${BOLD}Disks:${RESET}\n"
        for i in $(seq 0 $((DISK_COUNT - 1))); do
            DISK=$(echo "$CH_CFG" | jq ".disks[$i]")
            D_PATH=$(echo "$DISK" | jq -r '.path')
            D_RO=$(echo "$DISK" | jq '.readonly // false')
            D_SERIAL=$(echo "$DISK" | jq -r '.serial // "-"')
            D_TYPE=$(echo "$DISK" | jq -r '.image_type // "auto"')
            D_NQ=$(echo "$DISK" | jq '.num_queues // 0')
            D_QS=$(echo "$DISK" | jq '.queue_size // 0')
            D_DIRECT=$(echo "$DISK" | jq '.direct // false')
            D_SPARSE=$(echo "$DISK" | jq '.sparse // false')
            D_BACKING=$(echo "$DISK" | jq '.backing_files // false')
            printf "    [%d] %s\n" "$i" "$D_PATH"
            printf "        ro=%-5s serial=%-16s type=%-6s num_queues=%d queue_size=%d\n" \
                "$D_RO" "$D_SERIAL" "$D_TYPE" "$D_NQ" "$D_QS"
            printf "        direct=%-5s sparse=%-5s backing_files=%s\n" "$D_DIRECT" "$D_SPARSE" "$D_BACKING"
            if [ "$D_RO" = "false" ] && [ ! -f "$D_PATH" ]; then
                fail "COW disk missing: $D_PATH"
            fi
        done
    fi

    # Nets (from CH config)
    NET_COUNT=$(echo "$CH_CFG" | jq '.net | length // 0')
    if [ "$NET_COUNT" -gt 0 ]; then
        printf "\n  ${BOLD}Nets (CH config):${RESET}\n"
        for i in $(seq 0 $((NET_COUNT - 1))); do
            NET=$(echo "$CH_CFG" | jq ".net[$i]")
            N_TAP=$(echo "$NET" | jq -r '.tap // "-"')
            N_MAC=$(echo "$NET" | jq -r '.mac // "-"')
            N_NQ=$(echo "$NET" | jq -r '.num_queues // 0')
            N_QS=$(echo "$NET" | jq -r '.queue_size // 0')
            N_TSO=$(echo "$NET" | jq '.offload_tso // false')
            N_UFO=$(echo "$NET" | jq '.offload_ufo // false')
            N_CSUM=$(echo "$NET" | jq '.offload_csum // false')
            printf "    [%d] tap=%-8s mac=%s\n" "$i" "$N_TAP" "$N_MAC"
            printf "        num_queues=%-4d queue_size=%-4d offload: tso=%s ufo=%s csum=%s\n" \
                "$N_NQ" "$N_QS" "$N_TSO" "$N_UFO" "$N_CSUM"
        done
    fi
else
    # No API, no config.json — show what we have from DB.
    info "no live config (API socket absent, no config.json) — showing DB record"
    BOOT_CPUS=$(echo "$VM_REC" | jq '.config.cpu // 0')
    MEM=$(echo "$VM_REC" | jq '.config.memory // 0')
    MEM_MB=$((MEM / 1048576))
    STORAGE=$(echo "$VM_REC" | jq '.config.storage // 0')
    STOR_MB=$((STORAGE / 1048576))
    kv "CPUs" "$BOOT_CPUS"
    kv "Memory" "${MEM_MB} MiB"
    kv "Storage" "${STOR_MB} MiB"
    kv "Image" "$(echo "$VM_REC" | jq -r '.config.image // "-"')"
fi

# ---------------------------------------------------------------------------
# 5. Per-NIC Network Chain (veth ↔ tap inside netns)
# ---------------------------------------------------------------------------
header "Network Chain (per-NIC)"

NETNS_NAME="cocoon-${VM_ID}"
NETNS_PATH="/var/run/netns/${NETNS_NAME}"

# Read network configs from VM record
NIC_COUNT=$(echo "$VM_REC" | jq '.network_configs | length')

if [ "$NIC_COUNT" -eq 0 ]; then
    info "No NICs configured"
else
    kv "NIC count" "$NIC_COUNT"
    kv "Netns" "$NETNS_NAME"

    if [ ! -e "$NETNS_PATH" ]; then
        fail "Network namespace missing: $NETNS_PATH"
    else
        pass "Network namespace exists"

        for i in $(seq 0 $((NIC_COUNT - 1))); do
            NIC=$(echo "$VM_REC" | jq ".network_configs[$i]")
            N_TAP=$(echo "$NIC" | jq -r '.tap // "-"')
            N_MAC=$(echo "$NIC" | jq -r '.mac // "-"')
            N_NQ=$(echo "$NIC" | jq -r '.num_queues // 0')
            N_QS=$(echo "$NIC" | jq -r '.queue_size // 0')
            N_IP=$(echo "$NIC" | jq -r '.network.ip // "-"')
            N_GW=$(echo "$NIC" | jq -r '.network.gateway // "-"')
            N_PFX=$(echo "$NIC" | jq -r '.network.prefix // 0')

            printf "\n  ${BOLD}NIC %d:${RESET}\n" "$i"
            kv "  Expected tap" "$N_TAP"
            kv "  Expected MAC" "$N_MAC"
            kv "  CH num_queues" "$N_NQ"
            kv "  CH queue_size" "$N_QS"
            kv "  Allocated IP" "${N_IP}/${N_PFX}"
            kv "  Gateway" "$N_GW"

            VETH_NAME="eth${i}"

            # --- veth info ---
            printf "\n    ${CYAN}veth ($VETH_NAME):${RESET}\n"
            VETH_INFO=$(ip netns exec "$NETNS_NAME" ip -d link show "$VETH_NAME" 2>/dev/null || echo "")
            if [ -n "$VETH_INFO" ]; then
                VETH_MAC=$(echo "$VETH_INFO" | grep -oP 'link/ether \K[0-9a-f:]+' | head -1)
                VETH_STATE=$(echo "$VETH_INFO" | grep -oP 'state \K\w+' | head -1)
                VETH_MTU=$(echo "$VETH_INFO" | grep -oP 'mtu \K\d+' | head -1)
                kv "    MAC" "$VETH_MAC"
                kv "    State" "${VETH_STATE:-unknown}"
                kv "    MTU" "${VETH_MTU:-unknown}"

                # MAC consistency check
                if [ "$VETH_MAC" = "$N_MAC" ]; then
                    pass "  veth MAC matches CH config"
                else
                    fail "  veth MAC mismatch: veth=$VETH_MAC, CH=$N_MAC"
                fi

                # Check IP on veth (should be flushed)
                VETH_ADDRS=$(ip netns exec "$NETNS_NAME" ip -4 addr show dev "$VETH_NAME" 2>/dev/null | grep 'inet ' || true)
                if [ -z "$VETH_ADDRS" ]; then
                    pass "  veth IP flushed (correct — guest owns the IP)"
                else
                    warn "  veth has IP assigned (should be flushed): $VETH_ADDRS"
                fi
            else
                fail "  veth $VETH_NAME not found in netns $NETNS_NAME"
            fi

            # --- tap info ---
            printf "\n    ${CYAN}tap ($N_TAP):${RESET}\n"
            TAP_INFO=$(ip netns exec "$NETNS_NAME" ip -d link show "$N_TAP" 2>/dev/null || echo "")
            if [ -n "$TAP_INFO" ]; then
                TAP_STATE=$(echo "$TAP_INFO" | grep -oP 'state \K\w+' | head -1)
                TAP_MTU=$(echo "$TAP_INFO" | grep -oP 'mtu \K\d+' | head -1)
                kv "    State" "${TAP_STATE:-unknown}"
                kv "    MTU" "${TAP_MTU:-unknown}"

                # Check tap queue count via /sys
                TAP_QUEUES=""
                if ip netns exec "$NETNS_NAME" test -d "/sys/class/net/${N_TAP}/queues" 2>/dev/null; then
                    TX_Q=$(ip netns exec "$NETNS_NAME" ls "/sys/class/net/${N_TAP}/queues/" 2>/dev/null | grep -c '^tx-' || echo 0)
                    RX_Q=$(ip netns exec "$NETNS_NAME" ls "/sys/class/net/${N_TAP}/queues/" 2>/dev/null | grep -c '^rx-' || echo 0)
                    TAP_QUEUES="tx=$TX_Q rx=$RX_Q"
                    kv "    Queues" "$TAP_QUEUES"

                    # For multi-queue: num_queues = cpu*2, so TX queues = cpu, expected tap queues should match
                    # CH num_queues is the total (TX+RX pairs), tap TX count should be num_queues/2
                    EXPECTED_TX=$((N_NQ / 2))
                    if [ "$EXPECTED_TX" -le 0 ]; then
                        EXPECTED_TX=1
                    fi
                    if [ "$TX_Q" -eq "$EXPECTED_TX" ]; then
                        pass "  tap queue count matches CH config (tx=$TX_Q, expected=$EXPECTED_TX from num_queues=$N_NQ)"
                    else
                        fail "  tap queue mismatch: tx=$TX_Q, expected=$EXPECTED_TX (CH num_queues=$N_NQ)"
                    fi
                else
                    warn "  cannot read tap queue info from /sys"
                fi

                # MTU consistency
                if [ -n "$VETH_MTU" ] && [ -n "$TAP_MTU" ]; then
                    if [ "$VETH_MTU" = "$TAP_MTU" ]; then
                        pass "  MTU consistent: veth=$VETH_MTU tap=$TAP_MTU"
                    else
                        fail "  MTU mismatch: veth=$VETH_MTU tap=$TAP_MTU"
                    fi
                fi
            else
                fail "  tap $N_TAP not found in netns $NETNS_NAME"
            fi

            # --- TC filters ---
            printf "\n    ${CYAN}TC redirect:${RESET}\n"
            # Extract redirect target device from tc output.
            # tc prints: "mirred (Egress Redirect to device tap0) stolen"
            # We extract the word after "to device" and strip the trailing ")".
            extract_mirred_target() {
                sed -n 's/.*to device \([^ )]*\).*/\1/p' | head -1
            }

            # veth → tap
            VETH_TC=$(ip netns exec "$NETNS_NAME" tc filter show dev "$VETH_NAME" ingress 2>/dev/null || echo "")
            if echo "$VETH_TC" | grep -qi "mirred.*redirect"; then
                REDIRECT_TO=$(echo "$VETH_TC" | extract_mirred_target)
                if [ "$REDIRECT_TO" = "$N_TAP" ]; then
                    pass "  $VETH_NAME ingress → redirect to $REDIRECT_TO"
                elif [ -n "$REDIRECT_TO" ]; then
                    fail "  $VETH_NAME redirect target mismatch: expected=$N_TAP actual=$REDIRECT_TO"
                else
                    warn "  $VETH_NAME has mirred redirect but could not parse target device"
                fi
            else
                fail "  no TC redirect on $VETH_NAME ingress"
            fi

            # tap → veth
            TAP_TC=$(ip netns exec "$NETNS_NAME" tc filter show dev "$N_TAP" ingress 2>/dev/null || echo "")
            if echo "$TAP_TC" | grep -qi "mirred.*redirect"; then
                REDIRECT_TO=$(echo "$TAP_TC" | extract_mirred_target)
                if [ "$REDIRECT_TO" = "$VETH_NAME" ]; then
                    pass "  $N_TAP ingress → redirect to $REDIRECT_TO"
                elif [ -n "$REDIRECT_TO" ]; then
                    fail "  $N_TAP redirect target mismatch: expected=$VETH_NAME actual=$REDIRECT_TO"
                else
                    warn "  $N_TAP has mirred redirect but could not parse target device"
                fi
            else
                fail "  no TC redirect on $N_TAP ingress"
            fi
        done
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
printf "\n${BOLD}--- Done ---${RESET}\n"
