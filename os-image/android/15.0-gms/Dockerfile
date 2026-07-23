# cocoon OCI image: Android (Redroid) + GMS + ARM translation — direct boot.
#
# Adds to the stock 15.0 image:
#   - libndk_translation (Google's ARM→x86 translator, ChromeOS prebuilt):
#     arm64-only APKs (WeChat, Meituan, most CN apps) run on x86_64 hosts
#   - Google core suite (LiteGapps lite): GmsCore, Play Store, GSF, sync
#     adapters — Chrome and other Google apps install natively via Play login
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
ARG COCOON_AGENT_VERSION=0.2.0
ARG COCOON_AGENT_SHA256=73dc18e588828630f0ad5f3a1b86511f315d0df1f2efcbb409e821d258d45234

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
    ca-certificates curl unzip xz-utils \
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
      # binfmt_misc: ndk_translation registers ARM exec handlers through it
      echo binfmt_misc; \
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

# libndk_translation prebuilt: same source and pin as waydroid_script (extracted
# from ChromeOS by supremegamers); LiteGapps payload tree is <arch>/<api>/system.
ARG NDK_TRANSLATION_COMMIT=68734c52556d3d7a6db34c603dd9276915c29f2f
ARG NDK_TRANSLATION_MD5=0b2207c490fcb400aa5c87fcf0d52d38
ARG LITEGAPPS_URL="https://downloads.sourceforge.net/project/litegapps/litegapps/x86_64/35/lite/2024-10-27/LiteGapps-x86_64-15.0-20241027-official.zip"
ARG LITEGAPPS_SHA256=a868e988828487fb7d5fd74b3f527493a10e915e4a7c7f0cfb2e7d306e1d58cb

RUN curl -fsSL "https://github.com/supremegamers/vendor_google_proprietary_ndk_translation-prebuilt/archive/${NDK_TRANSLATION_COMMIT}.zip" -o /tmp/ndk.zip && \
    echo "${NDK_TRANSLATION_MD5}  /tmp/ndk.zip" | md5sum -c - && \
    unzip -q /tmp/ndk.zip -d /tmp/ndk && \
    cp -a /tmp/ndk/*/prebuilts/. /output/system/ && \
    test -f /output/system/lib64/libndk_translation.so && \
    test -f /output/system/etc/init/ndk_translation.rc && \
    curl -fsSL "${LITEGAPPS_URL}" -o /tmp/lg.zip && \
    echo "${LITEGAPPS_SHA256}  /tmp/lg.zip" | sha256sum -c - && \
    unzip -q /tmp/lg.zip 'files/*' -d /tmp/lg && \
    tar -xJf /tmp/lg/files/files.tar.xz -C /tmp/lg && \
    cp -a /tmp/lg/x86_64/35/system/. /output/system/ && \
    test -f /output/system/product/priv-app/GmsCore/GmsCore.apk && \
    test -f /output/system/product/priv-app/Phonesky/Phonesky.apk && \
    # init-wrapper relocates product/system_ext apps into /system at boot, but
    # priv-app allowlists are read per partition — mirror them into system/etc.
    cp -a /output/system/product/etc/permissions/. /output/system/etc/permissions/ && \
    cp -a /output/system/system_ext/etc/permissions/. /output/system/etc/permissions/ && \
    rm -rf /tmp/ndk.zip /tmp/ndk /tmp/lg.zip /tmp/lg

# The base ships native-bridge props but no translator: vendor names libnb.so
# (the winning definition) and system says 0 — point both at the real library.
COPY --from=redroid/redroid:15.0.0_64only-latest /vendor/build.prop /tmp/vendor-build.prop
COPY --from=redroid/redroid:15.0.0_64only-latest /system/build.prop /tmp/system-build.prop
RUN mkdir -p /output/vendor && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' /tmp/vendor-build.prop > /output/vendor/build.prop && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' /tmp/system-build.prop > /output/system/build.prop && \
    printf 'ro.ndk_translation.version=0.2.3\n' >> /output/system/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/vendor/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/system/build.prop

# ---- Stage 2: Android rootfs + single cocoon layer ----
FROM redroid/redroid:15.0.0_64only-latest

# Single layer: kernel, initrd, modules, /sbin/init wrapper, network fix, OMX fix, GMS, ARM translation.
COPY --from=builder /output/ /
