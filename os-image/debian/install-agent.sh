#!/bin/sh
# Vendored from cocoon/os-image/ubuntu/install-agent.sh at Cocoon v0.5.9
# (144927060c3e90dbe2f3e1a15143572c402958de), with pinned agent archives.
set -eu

AGENT_VERSION="0.2.0"
ARCH="${TARGETARCH:-$(dpkg --print-architecture)}"
case "$ARCH" in
    amd64)
        AGENT_ARCH="x86_64"
        AGENT_SHA256="73dc18e588828630f0ad5f3a1b86511f315d0df1f2efcbb409e821d258d45234"
        ;;
    arm64)
        AGENT_ARCH="arm64"
        AGENT_SHA256="548f4729a11797c4a6abe1b65d3791606a9a4f1de5592bf2ea367c573e0f6768"
        ;;
    *)
        printf "install-agent: unsupported architecture '%s' (expected amd64 or arm64)\n" "$ARCH" >&2
        exit 1
        ;;
esac

mkdir -p /run/sshd /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/00-cocoon-development.conf <<'EOF'
# INSECURE development-only compatibility access.
PermitRootLogin yes
PasswordAuthentication yes
EOF
systemctl enable ssh.service

TARBALL="cocoon-agent_${AGENT_VERSION}_Linux_${AGENT_ARCH}.tar.gz"
URL="https://github.com/cocoonstack/cocoon-agent/releases/download/v${AGENT_VERSION}/${TARBALL}"
TMP_TARBALL=$(mktemp)
trap 'rm -f "$TMP_TARBALL"' EXIT HUP INT TERM
curl -fsSL "$URL" -o "$TMP_TARBALL"
printf '%s  %s\n' "$AGENT_SHA256" "$TMP_TARBALL" | sha256sum -c -
tar -xz -C /usr/local/bin -f "$TMP_TARBALL" cocoon-agent
chmod 0755 /usr/local/bin/cocoon-agent

cat > /etc/systemd/system/cocoon-agent.service <<'EOF'
[Unit]
Description=Cocoon agent (vsock command exec)
Documentation=https://github.com/cocoonstack/cocoon-agent

[Service]
Type=simple
User=root
Group=root
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
