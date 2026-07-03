# cocoon OCI image: Android (Redroid) — direct boot, no Ubuntu/systemd layer.
#
# Architecture: Host → cocoon VM → Android init (PID 1)
# Boot flow:
#   CH --kernel vmlinuz --initramfs initrd.img --disk layer.erofs... --disk cow.ext4
#   → initramfs overlay.sh: mount erofs layers + cow → overlayfs rootfs
#   → switch_root → /sbin/init (busybox wrapper) → exec /init (Android init, PID 1)
#
# Remote access: adb/scrcpy to <vm-ip>:5555
#
# Layer optimization: All cocoon additions are staged into /output/ in the
# builder, then applied as a single COPY layer to minimize EROFS disk count.

# ---- Stage 1: kernel + initramfs + cocoon extras builder ----
FROM ubuntu:24.04 AS builder

ENV DEBIAN_FRONTEND=noninteractive

# Bumping COCOON_AGENT_VERSION must also update COCOON_AGENT_SHA256 — the
# sha256sum -c step below fails the build when they drift.
ARG COCOON_AGENT_VERSION=0.1.7
ARG COCOON_AGENT_SHA256=210f34043d1def6f06f7b4a3722feb1bc6e46368d6419d264b7bb03bfc22cda0

RUN --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    --mount=type=secret,id=cocoon_init_wrapper \
    --mount=type=secret,id=cocoon_network_rc \
    --mount=type=secret,id=cocoon_omx_fix_rc \
    --mount=type=secret,id=cocoon_disable_bt_rc \
    --mount=type=secret,id=cocoon_agent_rc \
    # --- Install kernel + initramfs tooling ---
    apt-get update && apt-get install -y --no-install-recommends \
    linux-image-generic \
    initramfs-tools \
    busybox-static \
    ca-certificates curl \
    && \
    apt-get install -y --no-install-recommends \
        linux-modules-extra-$(ls /lib/modules/ | head -1) \
    && \
    # --- cocoon overlay hook (mounts erofs layers + cow → overlayfs root) ---
    cp /run/secrets/cocoon_overlay /etc/initramfs-tools/scripts/cocoon-overlay && \
    chmod 0755 /etc/initramfs-tools/scripts/cocoon-overlay && \
    # --- initramfs modules ---
    { \
      echo erofs; echo overlay; echo ext4; \
      echo virtio_blk; echo virtio_pci; echo virtio_ring; echo virtio_net; \
      echo vsock; echo vmw_vsock_virtio_transport; \
      echo binder_linux; echo loop; \
    } >> /etc/initramfs-tools/modules && \
    # netfilter: Android netd requires iptables tables.
    KVER="$(ls /lib/modules | sort -V | tail -1)" && \
    for m in \
      ip_tables iptable_filter iptable_nat iptable_mangle \
      ip6_tables ip6table_filter ip6table_nat ip6table_mangle \
      nf_conntrack nf_nat nf_conntrack_netlink nfnetlink nfnetlink_log \
      xt_connmark xt_NFLOG xt_REJECT xt_state xt_u32 xt_mark xt_MASQUERADE \
    ; do \
      modinfo -k "$KVER" "$m" >/dev/null 2>&1 && echo "$m" >> /etc/initramfs-tools/modules || true; \
    done && \
    echo 'options binder_linux devices="binder,hwbinder,vndbinder"' \
        > /etc/modprobe.d/binder.conf && \
    sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf && \
    update-initramfs -u -k all && \
    # --- Stage all outputs into /output/ for single-layer COPY ---
    mkdir -p /output/boot /output/lib /output/sbin \
             /output/system/bin /output/system/etc/init && \
    cp /boot/vmlinuz-*    /output/boot/ && \
    cp /boot/initrd.img-* /output/boot/ && \
    cp -a /lib/modules    /output/lib/modules && \
    cp /bin/busybox        /output/sbin/busybox && \
    cp /run/secrets/cocoon_init_wrapper /output/sbin/init && \
    chmod 0755             /output/sbin/init && \
    cp /run/secrets/cocoon_network /output/system/bin/cocoon-network.sh && \
    chmod 0755             /output/system/bin/cocoon-network.sh && \
    cp /run/secrets/cocoon_network_rc /output/system/etc/init/cocoon-network.rc && \
    cp /run/secrets/cocoon_omx_fix_rc /output/system/etc/init/cocoon-omx-fix.rc && \
    cp /run/secrets/cocoon_disable_bt_rc /output/system/etc/init/cocoon-disable-bt.rc && \
    # --- cocoon-agent (vsock exec) — Android amd64 only ---
    cp /run/secrets/cocoon_agent_rc /output/system/etc/init/cocoon-agent.rc && \
    AGENT_TARBALL="cocoon-agent_${COCOON_AGENT_VERSION}_Linux_x86_64.tar.gz" && \
    curl -fsSL "https://github.com/cocoonstack/cocoon-agent/releases/download/v${COCOON_AGENT_VERSION}/${AGENT_TARBALL}" \
        -o "/tmp/${AGENT_TARBALL}" && \
    echo "${COCOON_AGENT_SHA256}  /tmp/${AGENT_TARBALL}" | sha256sum -c - && \
    tar -xz -C /output/system/bin/ -f "/tmp/${AGENT_TARBALL}" cocoon-agent && \
    rm -f "/tmp/${AGENT_TARBALL}" && \
    chmod 0755 /output/system/bin/cocoon-agent && \
    rm -rf /var/lib/apt/lists/*

# ---- Stage 2: Android rootfs + single cocoon layer ----
FROM redroid/redroid:14.0.0_64only-latest

# Single layer: kernel, initrd, modules, /sbin/init wrapper, network fix, OMX fix.
COPY --from=builder /output/ /
