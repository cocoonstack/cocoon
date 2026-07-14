#!/system/bin/sh
# Make Lawnchair the default HOME, retrying until it sticks. On first boot /data
# is empty and PackageManager scans /system/app lazily, so Lawnchair may not be a
# HOME candidate yet the instant sys.boot_completed fires; a single set-home then
# silently no-ops and stock launcher3 wins. Loop until resolve-activity confirms.
i=0
while [ "$i" -lt 90 ]; do
    pm disable-user --user 0 com.android.launcher3 2>/dev/null
    cmd package set-home-activity app.lawnchair/app.lawnchair.LawnchairLauncher 2>/dev/null
    home=$(cmd package resolve-activity --brief -c android.intent.category.HOME -a android.intent.action.MAIN 2>/dev/null | tail -n 1)
    case "$home" in
        *app.lawnchair/*) exit 0 ;;
    esac
    i=$((i + 1))
    sleep 2
done
