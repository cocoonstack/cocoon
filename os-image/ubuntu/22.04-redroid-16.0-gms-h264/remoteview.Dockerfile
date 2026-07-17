# Standalone remote-view helper: scrcpy 4.1 H.264 passthrough plus ordinary VNC
# fallback on one RFB port. The release binary is selected by TARGETARCH.
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG SCRCPY_RFB_REPO=cocoonstack/scrcpy-rfb
ARG SCRCPY_RFB_TAG=master
ARG SCRCPY_RFB_COMMIT

RUN apt-get update && apt-get install -y --no-install-recommends \
    adb curl ca-certificates libjpeg-turbo8 zlib1g && \
    case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac && \
    install -d /usr/local/share/scrcpy /tmp/scrcpy-rfb && \
    BASE="https://github.com/$SCRCPY_RFB_REPO/releases/download/$SCRCPY_RFB_TAG" && \
    ARTIFACT="scrcpy-rfb-linux-$TARGETARCH" && \
    curl -fsSL "$BASE/build-info.json" -o /tmp/scrcpy-rfb/build-info.json && \
    if [ -z "$SCRCPY_RFB_COMMIT" ]; then \
      SCRCPY_RFB_COMMIT="$(sed -n 's/.*"commit": "\([0-9a-f]\{40\}\)".*/\1/p' /tmp/scrcpy-rfb/build-info.json)"; \
    fi && \
    printf '%s' "$SCRCPY_RFB_COMMIT" | grep -Eq '^[0-9a-f]{40}$' && \
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
    rm -rf /var/lib/apt/lists/*

COPY remoteview-run.sh /usr/local/bin/remoteview-run.sh
RUN chmod +x /usr/local/bin/remoteview-run.sh

EXPOSE 5900
CMD ["/usr/local/bin/remoteview-run.sh"]
