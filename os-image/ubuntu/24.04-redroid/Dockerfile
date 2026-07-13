# Ubuntu 24.04 + Docker + Redroid — native redroid-in-container VM image.
#
# The redroid container is created AND started at BUILD time (build.sh, in a
# privileged DinD) and baked into /var/lib/docker with `--restart unless-stopped`,
# so the VM's dockerd auto-restarts it on boot — no docker run at first VM start.
# Networking is docker bridge with a published 5555 (NOT --network host): host
# net lets redroid's Android netd hijack the VM's netns routing and black-hole it.
#
# amd64 only (redroid + libndk are x86_64). daemon.json pins vfs + classic
# graphdriver; the VM's docker must match the DinD that generated docker-data.tar.
FROM ubuntu:24.04

ARG TARGETARCH
ARG DOCKER_CHANNEL=stable
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    --mount=type=secret,id=cocoon_install_agent \
    --mount=type=secret,id=daemon_json \
    apt-get update && apt-get install -y --no-install-recommends \
    linux-image-generic \
    initramfs-tools \
    systemd systemd-sysv systemd-timesyncd systemd-resolved \
    udev kmod iproute2 iputils-ping curl wget ca-certificates gnupg \
    openssh-server \
    && \
    apt-get install -y --no-install-recommends \
        linux-modules-extra-"$(ls /lib/modules/ | sort -V | tail -1)" && \
    # --- Docker engine (latest stable) from Docker's official apt repo ---
    install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    chmod a+r /etc/apt/keyrings/docker.asc && \
    ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble ${DOCKER_CHANNEL}" \
        > /etc/apt/sources.list.d/docker.list && \
    apt-get update && apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io && \
    mkdir -p /etc/docker && \
    cp /run/secrets/daemon_json /etc/docker/daemon.json && \
    systemctl enable docker.service containerd.service && \
    mkdir -p /var/lib/redroid-data && \
    # --- binder + netfilter for redroid, loaded in the running VM WITH the
    #     devices= option; not in initramfs, where an early no-option load would
    #     make the modules-load.d modprobe a no-op ---
    echo 'options binder_linux devices="binder,hwbinder,vndbinder"' > /etc/modprobe.d/binder.conf && \
    printf "binder_linux\nbinfmt_misc\nloop\n" > /etc/modules-load.d/redroid.conf && \
    KVER="$(ls /lib/modules | sort -V | tail -1)" && \
    for m in ip_tables iptable_filter iptable_nat iptable_mangle \
             ip6_tables ip6table_filter ip6table_nat \
             nf_conntrack nf_nat nfnetlink xt_MASQUERADE xt_mark xt_REJECT; do \
        modinfo -k "$KVER" "$m" >/dev/null 2>&1 && echo "$m" >> /etc/modules-load.d/redroid.conf || true; \
    done && \
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
    printf "[Match]\nName=e* v*\n[Network]\nDHCP=yes\n\n[DHCPv4]\nClientIdentifier=mac\n" > /etc/systemd/network/20-wired.network && \
    # --- SSH + cocoon-agent (vsock exec) ---
    sh /run/secrets/cocoon_install_agent && \
    echo 'root:cocoon' | chpasswd && \
    rm -rf /var/lib/apt/lists/*

# --- Remote view: scrcpy 4.1 (Android 15) + Xvfb + x11vnc + noVNC, a systemd
#     service that mirrors the baked redroid container to VNC :5900 and browser
#     noVNC :6080, and makes Lawnchair the default HOME once Android boots ---
RUN apt-get update && apt-get install -y --no-install-recommends \
    xvfb x11vnc novnc websockify openbox android-tools-adb \
    libgl1 libgles2 libegl1 mesa-libgallium libx11-6 libxext6 libxfixes3 \
    libsm6 libice6 libusb-1.0-0 && \
    curl -fsSL https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-linux-x86_64-v4.1.tar.gz \
        | tar xz -C /opt && \
    ln -s /opt/scrcpy-linux-x86_64-*/scrcpy /usr/local/bin/scrcpy && \
    rm -rf /var/lib/apt/lists/*
COPY 24.04-redroid/remoteview-run.sh /usr/local/bin/remoteview-run.sh
COPY 24.04-redroid/remoteview.service /etc/systemd/system/remoteview.service
RUN chmod +x /usr/local/bin/remoteview-run.sh && systemctl enable remoteview.service

# Baked docker state: the redroid image + a --restart unless-stopped container,
# started at build time, so dockerd auto-runs it on boot (no boot-time docker run).
ADD 24.04-redroid/docker-data.tar /var/lib/docker/

CMD ["/sbin/init"]
