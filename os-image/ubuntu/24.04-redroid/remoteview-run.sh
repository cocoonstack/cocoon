#!/bin/bash
# Remote view: scrcpy 4.1 (Android 15) mirrors the local redroid onto a virtual
# X display, served over VNC (:5900) and a browser (noVNC :6080). VNC input is
# forwarded by scrcpy back into Android.
set -eu

REDROID="${REDROID_ADB:-127.0.0.1:5555}"
VNC_PW="${VNC_PASSWORD:-redroid}"
RES="${VIEW_RES:-720x1280}"
DISP=":1"

adb start-server >/dev/null 2>&1 || true
adb connect "$REDROID" >/dev/null 2>&1 || true
for _ in $(seq 1 120); do
    [ "$(adb -s "$REDROID" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r\n ')" = "1" ] && break
    adb connect "$REDROID" >/dev/null 2>&1 || true
    sleep 3
done

# Make Lawnchair the default HOME (idempotent; the stock launcher3 otherwise wins).
adb -s "$REDROID" shell "cmd package set-home-activity app.lawnchair/app.lawnchair.LawnchairLauncher; pm disable-user --user 0 com.android.launcher3; input keyevent KEYCODE_HOME" >/dev/null 2>&1 || true

Xvfb "$DISP" -screen 0 "${RES}x24" -nolisten tcp &
export DISPLAY="$DISP"
export SDL_VIDEODRIVER=x11
sleep 2
openbox &
sleep 1

# scrcpy renders the device 1:1 (no --max-size) so it fills the display.
scrcpy -s "$REDROID" --no-audio --window-borderless --window-x=0 --window-y=0 \
    >/var/log/scrcpy.log 2>&1 &

mkdir -p "$HOME/.vnc"
x11vnc -storepasswd "$VNC_PW" "$HOME/.vnc/passwd" >/dev/null 2>&1
x11vnc -display "$DISP" -rfbauth "$HOME/.vnc/passwd" -forever -shared -noxdamage \
    -rfbport 5900 -bg -o /var/log/x11vnc.log
websockify --web /usr/share/novnc 6080 127.0.0.1:5900 >/var/log/websockify.log 2>&1 &

wait
