#!/system/bin/sh
# Ensure the curated GMS/Chrome set is enabled for the primary user, then prefer
# Fossify Launcher while keeping Launcher3 enabled as a recovery fallback.
TARGET=org.fossify.home/org.fossify.home.activities.MainActivity

for package in \
    com.google.android.gms \
    com.google.android.gsf \
    com.android.vending \
    com.google.android.onetimeinitializer \
    com.google.android.partnersetup \
    com.google.android.feedback \
    com.android.chrome \
    org.fossify.home; do
    cmd package install-existing --user 0 "$package" >/dev/null 2>&1 || true
    pm enable --user 0 "$package" >/dev/null 2>&1 || true
done

i=0
while [ "$i" -lt 90 ]; do
    cmd package set-home-activity "$TARGET" 2>/dev/null || true
    home=$(cmd package resolve-activity --brief \
        -c android.intent.category.HOME \
        -a android.intent.action.MAIN 2>/dev/null | tail -n 1)
    case "$home" in
        org.fossify.home/*) exit 0 ;;
    esac
    i=$((i + 1))
    sleep 2
done

exit 0
