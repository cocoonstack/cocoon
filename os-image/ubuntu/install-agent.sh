#!/bin/sh
# Install cocoon-agent (vsock exec) and sshd into a Debian/Ubuntu image.
# Caller is expected to have already installed `openssh-server` via apt
# in the same RUN, and to have curl available.
#
# Idempotent: re-running the script overwrites the binary and unit file,
# `systemctl enable` is a no-op when the symlinks are already in place.
set -eu

AGENT_VERSION="${COCOON_AGENT_VERSION:-0.1.5}"
ARCH="${TARGETARCH:-$(dpkg --print-architecture)}"
case "$ARCH" in
    amd64) AGENT_ARCH="x86_64"; AGENT_SHA256="f0fa49a65d0648795316898af351574777e74bf75b1a600b5046cb741d76e123" ;;
    arm64) AGENT_ARCH="arm64";  AGENT_SHA256="f2e69c6be8f7d44b8d21ecd6b91a950bc0aaae7c16e63bec91f010e2311d1b26" ;;
    *) echo "install-agent: unsupported arch '$ARCH'" >&2; exit 1 ;;
esac

# 1. sshd: permit root login (cocoon images use root:cocoon by default).
mkdir -p /run/sshd
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
systemctl enable ssh

# 2. cocoon-agent binary: pinned-version tarball from upstream releases.
# Per-arch SHA256 — bumping AGENT_VERSION without updating both checksums
# fails the sha256sum -c check instead of silently shipping a wrong binary.
TARBALL="cocoon-agent_${AGENT_VERSION}_Linux_${AGENT_ARCH}.tar.gz"
URL="https://github.com/cocoonstack/cocoon-agent/releases/download/v${AGENT_VERSION}/${TARBALL}"
TMP_TARBALL="$(mktemp)"
trap 'rm -f "$TMP_TARBALL"' EXIT
curl -fsSL "$URL" -o "$TMP_TARBALL"
echo "$AGENT_SHA256  $TMP_TARBALL" | sha256sum -c -
tar -xz -C /usr/local/bin/ -f "$TMP_TARBALL" cocoon-agent
chmod 0755 /usr/local/bin/cocoon-agent

# 3. systemd unit. Mirrors upstream packaging/cocoon-agent.service so the
#    in-VM service stays in sync with what cocoon-agent is tested against.
cat > /etc/systemd/system/cocoon-agent.service <<'EOF'
[Unit]
Description=Cocoon agent (vsock command exec)
Documentation=https://github.com/cocoonstack/cocoon-agent

[Service]
Type=simple
User=root
Group=root
# Best-effort load — most kernels build the transport in or auto-load on
# virtio-vsock device probe; the leading dash keeps the unit alive on
# minimal kernels (e.g. ubuntu linux-image-virtual).
ExecStartPre=-/sbin/modprobe vhost_vsock
ExecStart=/usr/local/bin/cocoon-agent serve
Environment=AGENT_LOG_LEVEL=info
Restart=always
RestartSec=2s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl enable cocoon-agent.service
