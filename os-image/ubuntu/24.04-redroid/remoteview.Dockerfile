# Standalone remote-view helper: scrcpy 4.1 H.264 passthrough plus ordinary VNC
# fallback on one RFB port. The release binary is selected by TARGETARCH.
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG SCRCPY_RFB_REPO=cocoonstack/libvncserver
ARG SCRCPY_RFB_TAG=dev
ARG SCRCPY_RFB_COMMIT
ARG SCRCPY_SERVER_SHA256=deacb991ed2509715160ffdc7907e47b4160eb30d1566217e9047fd5b8850cae

RUN apt-get update && apt-get install -y --no-install-recommends \
    adb curl ca-certificates libjpeg-turbo8 zlib1g && \
    test -n "$SCRCPY_RFB_COMMIT" && \
    case "$TARGETARCH" in amd64|arm64) ;; *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; esac && \
    install -d /usr/local/share/scrcpy /tmp/scrcpy-rfb && \
    curl -fsSL \
      https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-server-v4.1 \
      -o /usr/local/share/scrcpy/scrcpy-server.jar && \
    echo "$SCRCPY_SERVER_SHA256  /usr/local/share/scrcpy/scrcpy-server.jar" | sha256sum -c - && \
    BASE="https://github.com/$SCRCPY_RFB_REPO/releases/download/$SCRCPY_RFB_TAG" && \
    ARTIFACT="scrcpy-rfb-linux-$TARGETARCH" && \
    curl -fsSL "$BASE/build-info.json?commit=$SCRCPY_RFB_COMMIT" -o /tmp/scrcpy-rfb/build-info.json && \
    grep -Fq "\"commit\": \"$SCRCPY_RFB_COMMIT\"" /tmp/scrcpy-rfb/build-info.json && \
    curl -fsSL "$BASE/$ARTIFACT?commit=$SCRCPY_RFB_COMMIT" -o "/tmp/scrcpy-rfb/$ARTIFACT" && \
    curl -fsSL "$BASE/$ARTIFACT.sha256?commit=$SCRCPY_RFB_COMMIT" -o "/tmp/scrcpy-rfb/$ARTIFACT.sha256" && \
    cd /tmp/scrcpy-rfb && sha256sum -c "$ARTIFACT.sha256" && \
    install -m 0755 "$ARTIFACT" /usr/local/bin/scrcpy-rfb && \
    cd / && rm -rf /tmp/scrcpy-rfb && \
    rm -rf /var/lib/apt/lists/*

COPY remoteview-run.sh /usr/local/bin/remoteview-run.sh
RUN chmod +x /usr/local/bin/remoteview-run.sh

EXPOSE 5900
CMD ["/usr/local/bin/remoteview-run.sh"]
