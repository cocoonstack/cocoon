#!/bin/bash
set -e

# UUID v5 (RFC 4122, SHA-1 based): deterministic UUID from NAMESPACE_URL + name.
# Pure bash + sha1sum — no Python required.
uuid_v5() {
    local hash
    hash=$( { printf '\x6b\xa7\xb8\x10\x9d\xad\x11\xd1\x80\xb4\x00\xc0\x4f\xd4\x30\xc8'
              printf '%s' "$1"; } | sha1sum | cut -c1-32 )
    local vb; vb=$(printf '%02x' "$(( (16#${hash:16:2} & 0x3f) | 0x80 ))")
    printf '%s-%s-5%s-%s%s-%s\n' \
        "${hash:0:8}" "${hash:8:4}" "${hash:13:3}" \
        "$vb" "${hash:18:2}" "${hash:20:12}"
}

echo "=== Cocoon MicroVM Engine: Bootstrapping (Multi-Layer OCI) ==="

# 0. Check KVM
if [ ! -w "/dev/kvm" ]; then
    echo "Error: Access to /dev/kvm denied."
    exit 1
fi
ARCH=$(uname -m)

# 1. Install dependencies
_MISSING=""
command -v jq          >/dev/null 2>&1 || _MISSING="$_MISSING jq"
command -v mkfs.erofs  >/dev/null 2>&1 || _MISSING="$_MISSING erofs-utils"
command -v mkfs.ext4   >/dev/null 2>&1 || _MISSING="$_MISSING e2fsprogs"
command -v wget        >/dev/null 2>&1 || _MISSING="$_MISSING wget"
if [ -n "$_MISSING" ]; then
    echo "[1/7] Installing missing dependencies:$_MISSING"
    sudo apt-get install -y -qq $_MISSING
fi

# 2. Download Cloud Hypervisor
CH_VERSION="v51.0"
CH_BIN="./cloud-hypervisor"
if [ ! -x "$CH_BIN" ]; then
    [ "$ARCH" = "x86_64" ] && CH_URL="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static" \
    || CH_URL="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static-aarch64"
    wget -qO "$CH_BIN" "$CH_URL" && chmod +x "$CH_BIN"
fi
sudo setcap cap_net_admin+ep "$CH_BIN"

# 4. Extract Layers via Crane
if [ -n "$IMAGE_NAME" ]; then
    echo "[4/7] Extracting layers for '$IMAGE_NAME'..."
    rm -f layer*.erofs boot/vmlinuz* boot/initrd.img*
    
    # Download Crane
    if [ ! -x "./crane" ]; then
        [ "$ARCH" = "x86_64" ] && CR_ARCH="x86_64" || CR_ARCH="arm64"
        wget -qO- "https://github.com/google/go-containerregistry/releases/download/v0.19.1/go-containerregistry_Linux_${CR_ARCH}.tar.gz" | tar -xz crane
    fi

    ./crane pull "$IMAGE_NAME" image.tar
    rm -rf image_temp && mkdir -p image_temp boot
    tar -xf image.tar -C image_temp

    # Process layers (Bottom up)
    LAYERS=$(jq -r '.[0].Layers[]' image_temp/manifest.json)
    
    IDX=0
    for L in $LAYERS; do
        echo "      Layer $IDX -> layer${IDX}.erofs"
        tar -xf "image_temp/$L" boot/ 2>/dev/null || true
        # Compress layer to EROFS
        # UUID v5: deterministically derived from the layer path (which encodes the
        # content digest). Same OCI layer -> same UUID. Pure bash + sha1sum, no Python.
        LAYER_UUID=$(uuid_v5 "$L")
        if gzip -t "image_temp/$L" 2>/dev/null; then
            gzip -dc "image_temp/$L" | mkfs.erofs --tar=f --aufs -zlz4hc -C16384 -T0 -U "$LAYER_UUID" "layer${IDX}.erofs"
        else
            mkfs.erofs --tar=f --aufs -zlz4hc -C16384 -T0 -U "$LAYER_UUID" "layer${IDX}.erofs" "image_temp/$L"
        fi
        IDX=$((IDX+1))
    done
    rm -rf image_temp image.tar
fi

# 5. Prepare COW (Optimized for performance and sparse efficiency)
# lazy_itable_init=1 speeds up formatting on large sparse files
echo "[5/7] Preparing optimized sparse COW disk..."
truncate -s 10G cow.raw
mkfs.ext4 -F -m 0 -q -E lazy_itable_init=1,lazy_journal_init=1,discard cow.raw

# 6. Build Arguments
KERNEL=$(ls boot/vmlinuz-* | head -1)
INITRD=$(ls boot/initrd.img-* | head -1)
LAYER_FILES=$(ls -1 layer*.erofs | sort -V)

DISK_CONFIGS=()
COCOON_LAYERS=""
I=0
for f in $LAYER_FILES; do
    DISK_CONFIGS+=("path=$f,readonly=on,image_type=raw,num_queues=2,queue_size=256,serial=cocoon-layer${I}")
    # OverlayFS order: Top, ..., Bottom
    if [ -z "$COCOON_LAYERS" ]; then COCOON_LAYERS="cocoon-layer${I}"; else COCOON_LAYERS="cocoon-layer${I},${COCOON_LAYERS}"; fi
    I=$((I+1))
done

# COW Disk Optimization: 
# readonly layers keep host page cache; the writable COW disk uses direct=on
# to bypass host cache. sparse=on keeps the raw file hole-aware.
# num_queues=2 (Match boot CPUs), queue_size=256 (Deep queue for better throughput)
DISK_CONFIGS+=("path=cow.raw,readonly=off,direct=on,sparse=on,image_type=raw,num_queues=2,queue_size=256,serial=cocoon-cow")

# 7. Ignite
echo "[7/7] Igniting Cloud Hypervisor..."
"$CH_BIN" --kernel "$KERNEL" --initramfs "$INITRD" \
    --disk "${DISK_CONFIGS[@]}" \
    --cmdline "console=ttyS0 loglevel=3 boot=cocoon-overlay cocoon.layers=${COCOON_LAYERS} cocoon.cow=cocoon-cow clocksource=kvm-clock rw" \
    --cpus "boot=2,max=8" \
    --memory "size=1024M$([ "$(cat /proc/sys/vm/nr_hugepages 2>/dev/null)" -gt 0 ] && echo ',hugepages=on')" \
    --rng "src=/dev/urandom" \
    --watchdog \
    --balloon "size=512M,deflate_on_oom=on,free_page_reporting=on" \
    --serial tty --console off
