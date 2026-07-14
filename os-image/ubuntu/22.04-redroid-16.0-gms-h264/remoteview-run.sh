#!/bin/bash
# Remote view: scrcpy 4.1 produces H.264 and control sockets. scrcpy-rfb passes
# H.264 through to capable clients and provides
# Tight/JPEG/ZRLE/Hextile/Raw fallback on the same VNC port for ordinary
# clients. No X server is involved.
set -euo pipefail

REDROID="${REDROID_ADB:-127.0.0.1:5555}"
VNC_PW="${VNC_PASSWORD:-redroid}"
RFB_PORT="${RFB_PORT:-5900}"
SCRCPY_SERVER="${SCRCPY_SERVER:-/usr/local/share/scrcpy/scrcpy-server.jar}"
SCRCPY_PORT=27183
SCRCPY_SCID="${SCRCPY_SCID:-4a264fb0}"
VIDEO_BIT_RATE="${VIDEO_BIT_RATE:-8000000}"
MAX_FPS="${MAX_FPS:-60}"
server_pid=""
rfb_pid=""

cleanup() {
    [ -z "$rfb_pid" ] || kill "$rfb_pid" >/dev/null 2>&1 || true
    adb -s "$REDROID" forward --remove "tcp:$SCRCPY_PORT" >/dev/null 2>&1 || true
    adb -s "$REDROID" shell pkill -f \
        "com.genymobile.scrcpy.Server 4.1 scid=$SCRCPY_SCID" >/dev/null 2>&1 || true
    [ -z "$server_pid" ] || kill "$server_pid" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

adb start-server >/dev/null 2>&1 || true
adb connect "$REDROID" >/dev/null 2>&1 || true
boot_completed=false
for _ in $(seq 1 120); do
    if [ "$(adb -s "$REDROID" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r\n ')" = "1" ]; then
        boot_completed=true
        break
    fi
    adb connect "$REDROID" >/dev/null 2>&1 || true
    sleep 3
done
[ "$boot_completed" = true ] || { echo "Android boot timed out" >&2; exit 1; }

cleanup
adb -s "$REDROID" push "$SCRCPY_SERVER" /data/local/tmp/scrcpy-server.jar >/dev/null
adb -s "$REDROID" forward "tcp:$SCRCPY_PORT" "localabstract:scrcpy_$SCRCPY_SCID"
adb -s "$REDROID" shell \
    CLASSPATH=/data/local/tmp/scrcpy-server.jar \
    app_process / com.genymobile.scrcpy.Server 4.1 \
    "scid=$SCRCPY_SCID" log_level=info tunnel_forward=true \
    audio=false send_device_meta=false send_dummy_byte=false \
    video_codec=h264 "video_bit_rate=$VIDEO_BIT_RATE" "max_fps=$MAX_FPS" \
    'video_codec_options=profile=1,i-frame-interval=1' cleanup=false &
server_pid=$!

socket_ready=false
for _ in $(seq 1 100); do
    if adb -s "$REDROID" shell cat /proc/net/unix 2>/dev/null \
        | grep -q "@scrcpy_$SCRCPY_SCID"; then
        socket_ready=true
        break
    fi
    sleep 0.1
done
[ "$socket_ready" = true ] || { echo "scrcpy-server socket did not become ready" >&2; exit 1; }

/usr/local/bin/scrcpy-rfb \
    -rfbport "$RFB_PORT" \
    -passwd "$VNC_PW" \
    -desktop 'ReDroid scrcpy H.264 passthrough' &
rfb_pid=$!

wait -n "$server_pid" "$rfb_pid"
