# Ubuntu 22.04 + Docker + ReDroid 16 GMS — native redroid-in-container VM image.
#
# The ReDroid container is created at BUILD time (build.sh, in a privileged DinD)
# with its `--restart unless-stopped` policy armed by a one-shot wrapper that
# sleeps instead of booting Android, then baked into /var/lib/docker. The VM's
# dockerd replays it on boot and the wrapper execs Android /init — no first-boot
# docker create and no launcher service. binder+ashmem load via a docker.service
# ExecStartPre drop-in (the exported rootfs is container-detected, so
# modules-load.d is skipped). Networking is docker bridge with a
# published 5555 (NOT --network host): host net lets redroid's Android netd hijack
# the VM's netns routing and black-hole it.
#
# Per-arch build (TARGETARCH): amd64 bakes an x86_64 ReDroid + libndk ARM
# translation; arm64 bakes a native arm64 ReDroid with no translator. daemon.json
# pins the classic vfs graphdriver: the Cocoon root is itself overlayfs, on which
# docker's overlay2 (overlay-as-upperdir) is kernel-refused, and fuse-overlayfs
# leaves ReDroid's writable root unbootable after a same-container restart.
FROM ubuntu:22.04

ARG TARGETARCH
ARG DOCKER_CHANNEL=stable
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    --mount=type=secret,id=cocoon_install_agent \
    --mount=type=secret,id=daemon_json \
    APT_OPTS='-o Acquire::Retries=10 -o Acquire::http::Timeout=120' && \
    # The emulated arm64 apt hits ports.ubuntu.com, which times out intermittently
    # from CI: apt-get update then only warns and exits 0, leaving an empty index
    # so the install fails "Unable to locate package". Add a reliable MIT mirror as
    # a redundant source (apt fetches indexes from both, so either responding is
    # enough) and retry update+install as ONE unit, so a failed install re-runs
    # update instead of hammering a stale/empty package list.
    if [ "$TARGETARCH" = arm64 ]; then sed 's|http://ports.ubuntu.com/ubuntu-ports|http://mirrors.mit.edu/ubuntu-ports|g' /etc/apt/sources.list > /etc/apt/sources.list.d/mit-ports.list; fi && \
    apt_ensure() { for i in 1 2 3 4 5 6 7 8; do apt-get $APT_OPTS update && apt-get $APT_OPTS install -y --no-install-recommends "$@" && return 0; echo "apt retry $i for: $*" >&2; sleep 15; done; return 1; } && \
    apt_ensure \
    linux-image-virtual \
    initramfs-tools \
    systemd systemd-sysv systemd-timesyncd \
    udev kmod iproute2 iputils-ping curl wget ca-certificates gnupg \
    openssh-server \
    && \
    apt_ensure \
        linux-modules-extra-"$(ls /lib/modules/ | sort -V | tail -1)" && \
    # --- Docker engine (latest stable) from Docker's official apt repo ---
    install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    chmod a+r /etc/apt/keyrings/docker.asc && \
    ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu jammy ${DOCKER_CHANNEL}" \
        > /etc/apt/sources.list.d/docker.list && \
    apt_ensure \
        docker-ce docker-ce-cli containerd.io && \
    mkdir -p /etc/docker && \
    cp /run/secrets/daemon_json /etc/docker/daemon.json && \
    systemctl enable docker.service containerd.service && \
    mkdir -p /var/lib/redroid-data && \
    # --- binder + netfilter for redroid, loaded in the running VM WITH the
    #     devices= option; not in initramfs, where an early no-option load would
    #     make the modules-load.d modprobe a no-op ---
    echo 'options binder_linux devices="binder,hwbinder,vndbinder"' > /etc/modprobe.d/binder.conf && \
    printf "ashmem_linux\nbinder_linux\nbinfmt_misc\nloop\n" > /etc/modules-load.d/redroid.conf && \
    KVER="$(ls /lib/modules | sort -V | tail -1)" && \
    for m in ip_tables iptable_filter iptable_nat iptable_mangle \
             ip6_tables ip6table_filter ip6table_nat \
             nf_conntrack nf_nat nfnetlink xt_MASQUERADE xt_mark xt_REJECT; do \
        modinfo -k "$KVER" "$m" >/dev/null 2>&1 && echo "$m" >> /etc/modules-load.d/redroid.conf || true; \
    done && \
    # The exported rootfs keeps a /.dockerenv, so in the VM systemd-detect-virt is
    # "docker" and systemd-modules-load.service (ConditionVirtualization=!container)
    # is skipped — modules-load.d never runs. Load ReDroid's modules explicitly as
    # a docker ExecStartPre so binder/ashmem are up before dockerd replays the
    # baked container. binder_linux picks up devices= from modprobe.d/binder.conf.
    mkdir -p /etc/systemd/system/docker.service.d && \
    printf '[Service]\nExecStartPre=/sbin/modprobe ashmem_linux\nExecStartPre=/sbin/modprobe binder_linux\nExecStartPre=-/sbin/modprobe binfmt_misc\nExecStartPre=-/sbin/modprobe loop\n' \
        > /etc/systemd/system/docker.service.d/redroid-modules.conf && \
    # --- Cocoon initramfs overlay + network hooks ---
    cp /run/secrets/cocoon_overlay /etc/initramfs-tools/scripts/cocoon-overlay && \
    chmod 0755 /etc/initramfs-tools/scripts/cocoon-overlay && \
    cp /run/secrets/cocoon_network /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    chmod 0755 /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    printf "erofs\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\nvsock\nvmw_vsock_virtio_transport\n" >> /etc/initramfs-tools/modules && \
    sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf && \
    sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf && \
    echo 'IP=off' >> /etc/initramfs-tools/initramfs.conf && \
    update-initramfs -u -k all && \
    # --- systemd shift-left (skip fsck/remount on the stealth overlay root) ---
    truncate -s 0 /etc/fstab && \
    systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service && \
    systemctl enable systemd-networkd systemd-resolved systemd-timesyncd && \
    mkdir -p /etc/systemd/network && \
    # networkd must NOT touch docker's veth/bridge: the 20-wired Name=e* v* would
    # grab veth*, race docker's enslavement, and leave the container's veth off
    # docker0 (linkdown -> no container network). A higher-priority Unmanaged rule
    # (05- < 20-) shields them so docker keeps them enslaved.
    printf "[Match]\nName=veth* docker0 br-*\n\n[Link]\nUnmanaged=yes\n" > /etc/systemd/network/05-docker-unmanaged.network && \
    printf "[Match]\nName=e* v*\n[Network]\nDHCP=yes\n\n[DHCPv4]\nClientIdentifier=mac\n" > /etc/systemd/network/20-wired.network && \
    # --- SSH + cocoon-agent (vsock exec) ---
    sh /run/secrets/cocoon_install_agent && \
    echo 'root:cocoon' | chpasswd && \
    rm -rf /var/lib/apt/lists/*

# --- Remote view: scrcpy 4.1 H.264 is passed through directly to RFB encoding
#     50. Non-H.264 clients fall back to a lazily decoded
#     Tight/JPEG/ZRLE/Hextile/Raw framebuffer on the same port. There is no X
#     server or framebuffer scraping hop. ---
ARG SCRCPY_RFB_REPO=cocoonstack/scrcpy-rfb
ARG SCRCPY_RFB_TAG=master
# Bump to adopt a new scrcpy-rfb release; kept out of the top ARG block because an
# in-scope ARG joins every later cache key, so a bump there rebuilds the whole image.
ARG SCRCPY_RFB_COMMIT=00d1cab5b8c626e8361e5539a861c2668a2e4b76
RUN APT_OPTS='-o Acquire::Retries=10 -o Acquire::http::Timeout=120' && \
    if [ "$TARGETARCH" = arm64 ]; then sed 's|http://ports.ubuntu.com/ubuntu-ports|http://mirrors.mit.edu/ubuntu-ports|g' /etc/apt/sources.list > /etc/apt/sources.list.d/mit-ports.list; fi && \
    apt_ensure() { for i in 1 2 3 4 5 6 7 8; do apt-get $APT_OPTS update && apt-get $APT_OPTS install -y --no-install-recommends "$@" && return 0; echo "apt retry $i for: $*" >&2; sleep 15; done; return 1; } && \
    apt_ensure \
    adb libjpeg-turbo8 zlib1g && \
    case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac && \
    install -d /usr/local/share/scrcpy /tmp/scrcpy-rfb && \
    BASE="https://github.com/$SCRCPY_RFB_REPO/releases/download/$SCRCPY_RFB_TAG" && \
    ARTIFACT="scrcpy-rfb-linux-$TARGETARCH" && \
    curl -fsSL "$BASE/build-info.json" -o /tmp/scrcpy-rfb/build-info.json && \
    RELEASE_COMMIT="$(sed -n 's/.*"commit": "\([0-9a-f]\{40\}\)".*/\1/p' /tmp/scrcpy-rfb/build-info.json)" && \
    if [ -z "$SCRCPY_RFB_COMMIT" ]; then SCRCPY_RFB_COMMIT="$RELEASE_COMMIT"; fi && \
    printf '%s' "$SCRCPY_RFB_COMMIT" | grep -Eq '^[0-9a-f]{40}$' && \
    { [ "$SCRCPY_RFB_COMMIT" = "$RELEASE_COMMIT" ] || \
      { echo "scrcpy-rfb $SCRCPY_RFB_TAG is at $RELEASE_COMMIT, not the pinned $SCRCPY_RFB_COMMIT" >&2; exit 1; }; } && \
    cd /tmp/scrcpy-rfb && \
    for f in "$ARTIFACT" scrcpy-server-secure.jar "su1000-linux-$TARGETARCH"; do \
      curl -fsSL "$BASE/$f?commit=$SCRCPY_RFB_COMMIT" -o "$f" && \
      curl -fsSL "$BASE/$f.sha256?commit=$SCRCPY_RFB_COMMIT" -o "$f.sha256" && \
      sha256sum -c "$f.sha256"; \
    done && \
    install -m 0755 "$ARTIFACT" /usr/local/bin/scrcpy-rfb && \
    install -m 0644 scrcpy-server-secure.jar /usr/local/share/scrcpy/scrcpy-server.jar && \
    install -m 0755 "su1000-linux-$TARGETARCH" /usr/local/share/scrcpy/su1000 && \
    cd / && rm -rf /tmp/scrcpy-rfb && \
    apt-get purge -y --auto-remove curl wget gnupg && \
    apt-get clean && \
    rm -rf \
      /var/lib/apt/lists/* \
      /var/cache/apt/* \
      /var/cache/debconf/* \
      /usr/share/man/* \
      /usr/share/info/* \
      /tmp/* /var/tmp/* && \
    find /var/log -type f -exec truncate -s 0 {} +
COPY 22.04-redroid-16.0-gms-h264/remoteview-run.sh /usr/local/bin/remoteview-run.sh
COPY 22.04-redroid-16.0-gms-h264/remoteview.service /etc/systemd/system/remoteview.service
RUN chmod +x /usr/local/bin/remoteview-run.sh && \
    systemctl enable remoteview.service

# Baked Docker state: the single-layer ReDroid 16 GMS image PLUS a
# `--restart unless-stopped` container created and started at build time, so the
# VM's dockerd replays it on boot with no docker create/run at VM start. The vfs
# container-rootfs copy lives in the shared immutable EROFS (paid once per image,
# not per VM); the per-VM COW holds only the container's runtime writes.
ADD 22.04-redroid-16.0-gms-h264/docker-data-${TARGETARCH}.tar /var/lib/docker/
COPY 22.04-redroid-16.0-gms-h264/verify-vm-inner.sh /usr/local/libexec/cocoon/verify-vm-inner.sh
RUN chmod 0755 /usr/local/libexec/cocoon/verify-vm-inner.sh && \
    /usr/local/libexec/cocoon/verify-vm-inner.sh

CMD ["/sbin/init"]
