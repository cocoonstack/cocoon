# Remote-view helper: scrcpy 4.1 + Xvfb + x11vnc + noVNC. Mirrors a redroid
# device (adb) to VNC :5900 and browser noVNC :6080. Used standalone for dev and
# baked into the VM image for the shipped remote view.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ARG SCRCPY_URL="https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-linux-x86_64-v4.1.tar.gz"

RUN apt-get update && apt-get install -y --no-install-recommends \
    xvfb x11vnc novnc websockify openbox \
    adb curl ca-certificates \
    libgl1 libgles2 libegl1 mesa-libgallium libx11-6 libxext6 libxfixes3 \
    libsm6 libice6 libusb-1.0-0 \
    && \
    curl -fsSL "$SCRCPY_URL" | tar xz -C /opt && \
    ln -s /opt/scrcpy-linux-x86_64-*/scrcpy /usr/local/bin/scrcpy && \
    rm -rf /var/lib/apt/lists/*

COPY remoteview-run.sh /usr/local/bin/remoteview-run.sh
RUN chmod +x /usr/local/bin/remoteview-run.sh

EXPOSE 5900 6080
CMD ["/usr/local/bin/remoteview-run.sh"]
