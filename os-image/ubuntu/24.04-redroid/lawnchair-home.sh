#!/system/bin/sh
# Make Lawnchair the default HOME and its icon pack Lawnicons on first boot.
LC=/data/data/app.lawnchair
PREF=$LC/shared_prefs/com.android.launcher3.prefs.xml

# 1. Default HOME = Lawnchair, retried until it sticks. On first boot /data is empty
# and PackageManager scans /system/app lazily, so a one-shot set-home no-ops and
# stock launcher3 wins; loop until resolve-activity confirms Lawnchair.
i=0
while [ "$i" -lt 90 ]; do
    pm disable-user --user 0 com.android.launcher3 2>/dev/null
    cmd package set-home-activity app.lawnchair/app.lawnchair.LawnchairLauncher 2>/dev/null
    home=$(cmd package resolve-activity --brief -c android.intent.category.HOME -a android.intent.action.MAIN 2>/dev/null | tail -n 1)
    case "$home" in
        *app.lawnchair/*) break ;;
    esac
    i=$((i + 1))
    sleep 2
done

# 2. Icon pack = Lawnicons. Writing the pref directly does NOT trigger Lawnchair's
# icon-pack loader — only the in-app selection does — so drive the Settings UI
# (General > Icon style > Lawnicons) and confirm via the pref it writes, retrying
# if a tap misses. Coords are for redroid's fixed 720x1280 surface.
sleep 15
k=0
while [ "$k" -lt 6 ]; do
    grep -q app.lawnchair.lawnicons "$PREF" 2>/dev/null && break
    am start -n app.lawnchair/app.lawnchair.ui.preferences.PreferenceActivity
    sleep 3
    input tap 360 440
    sleep 2
    input swipe 360 950 360 250 400
    sleep 2
    input tap 360 1090
    sleep 2
    input tap 315 897
    sleep 3
    input keyevent KEYCODE_HOME
    sleep 3
    k=$((k + 1))
done
