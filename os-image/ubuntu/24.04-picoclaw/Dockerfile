FROM ubuntu:24.04

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    --mount=type=secret,id=picoclaw_service \
    --mount=type=secret,id=config_json \
    --mount=type=secret,id=picoclaw_init \
    apt-get update && apt-get install -y --no-install-recommends \
    linux-image-virtual \
    initramfs-tools \
    systemd systemd-sysv systemd-timesyncd systemd-resolved \
    udev kmod iproute2 iputils-ping curl wget arping tcpdump \
    software-properties-common htop ncdu nload net-tools \
    openssh-server \
    openbox \
    xserver-xorg-core dbus-x11 xrdp xorgxrdp \
    # Chinese language support
    locales fonts-noto-cjk fonts-noto-cjk-extra \
    python3 \
    && \
    # Locale: zh_CN.UTF-8 + en_US.UTF-8
    sed -i 's/# zh_CN.UTF-8/zh_CN.UTF-8/' /etc/locale.gen && \
    sed -i 's/# en_US.UTF-8/en_US.UTF-8/' /etc/locale.gen && \
    locale-gen && \
    # Timezone: SGT +8
    ln -sf /usr/share/zoneinfo/Asia/Singapore /etc/localtime && \
    echo "Asia/Singapore" > /etc/timezone && \
    # Chrome (amd64: google-chrome, arm64: chromium)
    ARCH=${TARGETARCH:-$(dpkg --print-architecture)}; \
    if [ "$ARCH" = "amd64" ]; then \
        wget "https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb" && \
        apt-get install -y "./google-chrome-stable_current_amd64.deb" && \
        rm "google-chrome-stable_current_amd64.deb" && \
        mkdir -p /root/.config/openbox && \
        echo 'google-chrome --start-maximized --no-sandbox --user-data-dir=/root/.chrome-data --disable-gpu --disable-software-rasterizer --disable-dev-shm-usage --disable-smooth-scrolling --remote-debugging-port=9222 &' > /root/.config/openbox/autostart; \
    else \
        add-apt-repository -y ppa:xtradeb/apps && \
        apt-get update && \
        apt-get install -y chromium && \
        mkdir -p /root/.config/openbox && \
        echo 'chromium --start-maximized --no-sandbox --user-data-dir=/root/.chromium-data --disable-gpu --disable-software-rasterizer --disable-dev-shm-usage --disable-smooth-scrolling --remote-debugging-port=9222 &' > /root/.config/openbox/autostart; \
    fi && \
    # PicoClaw: install pre-built binary
    ARCH=${TARGETARCH:-$(dpkg --print-architecture)}; \
    case "$ARCH" in \
        amd64) PC_ARCH="x86_64" ;; \
        arm64) PC_ARCH="arm64" ;; \
        *) echo "unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/sipeed/picoclaw/releases/latest/download/picoclaw_Linux_${PC_ARCH}.tar.gz" | tar xz -C /usr/local/bin/ picoclaw && \
    chmod 0755 /usr/local/bin/picoclaw && \
    # Cocoon initramfs overlay + network
    cp /run/secrets/cocoon_overlay /etc/initramfs-tools/scripts/cocoon-overlay && \
    chmod 0755 /etc/initramfs-tools/scripts/cocoon-overlay && \
    cp /run/secrets/cocoon_network /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    chmod 0755 /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    printf "erofs\nlz4\nlz4_compress\nzstd\nzstd_compress\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\nvirtio_gpu\n" >> /etc/initramfs-tools/modules && \
    sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf && \
    sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf && \
    echo 'IP=off' >> /etc/initramfs-tools/initramfs.conf && \
    update-initramfs -u -k all && \
    # Systemd tweaks
    truncate -s 0 /etc/fstab && \
    systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service && \
    systemctl enable systemd-networkd systemd-resolved systemd-timesyncd && \
    mkdir -p /etc/systemd/network && \
    printf "[Match]\nName=e* v*\n[Network]\nDHCP=yes\n" > /etc/systemd/network/20-wired.network && \
    # Openbox + xrdp
    echo "openbox-session" > /root/.xsession && \
    sed -i 's/test -x \/etc\/X11\/Xsession && exec \/etc\/X11\/Xsession/exec openbox-session/g' /etc/xrdp/startwm.sh && \
    sed -i 's/^max_bpp=.*/max_bpp=16/' /etc/xrdp/xrdp.ini && \
    sed -i 's/^crypt_level=.*/crypt_level=none/' /etc/xrdp/xrdp.ini && \
    sed -i 's/^tcp_nodelay=.*/tcp_nodelay=true/' /etc/xrdp/xrdp.ini && \
    adduser xrdp ssl-cert && \
    systemctl enable xrdp && \
    # SSH: enable root login
    mkdir -p /run/sshd && \
    sed -i 's/^#*PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config && \
    systemctl enable ssh && \
    # Root password
    echo 'root:cocoon' | chpasswd && \
    # PicoClaw: config + service + init script (NOT enabled until picoclaw-init runs)
    cp /run/secrets/picoclaw_service /etc/systemd/system/picoclaw.service && \
    mkdir -p /root/.picoclaw/workspace && \
    cp /run/secrets/config_json /root/.picoclaw/config.json && \
    cp /run/secrets/picoclaw_init /usr/local/bin/picoclaw-init && \
    chmod 0755 /usr/local/bin/picoclaw-init && \
    # Cleanup
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /var/lib/apt/lists/partial /var/cache/apt/archives/partial

CMD ["/sbin/init"]
