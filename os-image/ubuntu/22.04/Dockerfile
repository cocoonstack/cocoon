# Use the latest Ubuntu 22.04 (Jammy) LTS
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Combined System Setup (Single Layer)
# Install packages first so /etc/initramfs-tools/scripts/ exists, then inject the hook.
RUN --mount=type=secret,id=cocoon_overlay \
    --mount=type=secret,id=cocoon_network \
    apt-get update && apt-get install -y --no-install-recommends \
        linux-image-virtual \
        initramfs-tools \
        systemd \
        systemd-sysv \
        systemd-timesyncd \
        udev \
        kmod \
        iproute2 \
    && \
    cp /run/secrets/cocoon_overlay /etc/initramfs-tools/scripts/cocoon-overlay && \
    chmod 0755 /etc/initramfs-tools/scripts/cocoon-overlay && \
    cp /run/secrets/cocoon_network /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    chmod 0755 /etc/initramfs-tools/scripts/init-bottom/cocoon-network && \
    # [Kernel Config]
    printf "erofs\noverlay\next4\nvirtio_blk\nvirtio_pci\nvirtio_ring\nvirtio_net\n" >> /etc/initramfs-tools/modules && \
    sed -i 's/^COMPRESS=.*/COMPRESS=gzip/' /etc/initramfs-tools/initramfs.conf && \
    # [Networking] IP=off prevents initramfs from running DHCP during boot.
    # Kernel ip= parameters (when present) override this and still trigger ipconfig.
    sed -i '/^IP=/d' /etc/initramfs-tools/initramfs.conf && \
    echo 'IP=off' >> /etc/initramfs-tools/initramfs.conf && \
    # [Generate Initramfs] The secret is mounted and used here
    update-initramfs -u -k all && \
    # [Shift-Left System Hacks]
    truncate -s 0 /etc/fstab && \
    systemctl mask systemd-fsck-root.service systemd-remount-fs.service systemd-fsck@.service && \
    systemctl enable systemd-networkd systemd-resolved systemd-timesyncd && \
    mkdir -p /etc/systemd/network && \
    printf "[Match]\nName=e* v*\n[Network]\nDHCP=yes\n" > /etc/systemd/network/20-wired.network && \
    # [Final Touches]
    echo 'root:cocoon' | chpasswd && \
    rm -rf /var/lib/apt/lists/*

CMD ["/sbin/init"]