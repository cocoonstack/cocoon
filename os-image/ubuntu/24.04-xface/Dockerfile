FROM ubuntu:24.04

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    apt-get update && apt-get install -y --no-install-recommends \
    linux-image-virtual \
    initramfs-tools \
    systemd systemd-sysv systemd-timesyncd systemd-resolved \
    udev kmod iproute2 iputils-ping curl wget arping tcpdump \
    software-properties-common htop ncdu nload net-tools \
    xfce4 xfce4-terminal xfce4-screenshooter \
    xserver-xorg-core dbus-x11 xrdp xorgxrdp \
    && \
    ARCH=${TARGETARCH:-$(dpkg --print-architecture)}; \
    if [ "$ARCH" = "amd64" ]; then \
        wget "https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb" && \
        apt-get install -y "./google-chrome-stable_current_amd64.deb" && \
        rm "google-chrome-stable_current_amd64.deb" && \
        sed -i 's/exec -a "$0" "$HERE\/chrome" "$@"/exec -a "$0" "$HERE\/chrome" "$@" --no-sandbox --user-data-dir --disable-gpu --disable-software-rasterizer --disable-dev-shm-usage --disable-smooth-scrolling/ ' /opt/google/chrome/google-chrome && \
        mkdir -p /etc/xdg/autostart && \
        cp /usr/share/applications/google-chrome.desktop /etc/xdg/autostart/; \
    else \
        add-apt-repository -y ppa:xtradeb/apps && \
        apt-get update && \
        apt-get install -y chromium && \
        sed -i 's/^Exec=chromium.*/Exec=chromium --no-sandbox --user-data-dir --disable-gpu --disable-software-rasterizer --disable-dev-shm-usage --disable-smooth-scrolling %U/' /usr/share/applications/chromium.desktop && \
        mkdir -p /etc/xdg/autostart && \
        cp /usr/share/applications/chromium.desktop /etc/xdg/autostart/; \
    fi && \
    cp /run/secrets/cocoon_overlay /etc/initramfs-tools/scripts/cocoon-overlay && \
    chmod 0755 /etc/initramfs-tools/scripts/cocoon-overlay && \
    cp /run/secrets/cocoon_network /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    chmod 0755 /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    printf "erofs\nlz4\nlz4_compress\nzstd\nzstd_compress\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\nvirtio_gpu\n" >> /etc/initramfs-tools/modules && \
    sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf && \
    sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf && \
    echo 'IP=off' >> /etc/initramfs-tools/initramfs.conf && \
    update-initramfs -u -k all && \
    truncate -s 0 /etc/fstab && \
    systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service && \
    systemctl enable systemd-networkd systemd-resolved systemd-timesyncd && \
    mkdir -p /etc/systemd/network && \
    printf "[Match]\nName=e* v*\n[Network]\nDHCP=yes\n" > /etc/systemd/network/20-wired.network && \
    echo "xfce4-session" > /root/.xsession && \
    sed -i 's/test -x \/etc\/X11\/Xsession && exec \/etc\/X11\/Xsession/exec \/usr\/bin\/startxfce4/g' /etc/xrdp/startwm.sh && \
    sed -i 's/^max_bpp=.*/max_bpp=16/' /etc/xrdp/xrdp.ini && \
    sed -i 's/^crypt_level=.*/crypt_level=none/' /etc/xrdp/xrdp.ini && \
    sed -i 's/^tcp_nodelay=.*/tcp_nodelay=true/' /etc/xrdp/xrdp.ini && \
    mkdir -p /root/.config/xfce4/xfconf/xfce-perchannel-xml && \
    printf '<?xml version="1.0" encoding="UTF-8"?>\n<channel name="xfwm4" version="1.0">\n  <property name="general" type="empty">\n    <property name="use_compositing" type="bool" value="false"/>\n  </property>\n</channel>\n' > /root/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml && \
    adduser xrdp ssl-cert && \
    systemctl enable xrdp && \
    echo 'root:cocoon' | chpasswd && \
    rm -rf /var/lib/apt/lists/* && \
    mkdir -p /var/lib/apt/lists/partial /var/cache/apt/archives/partial

CMD ["/sbin/init"]
