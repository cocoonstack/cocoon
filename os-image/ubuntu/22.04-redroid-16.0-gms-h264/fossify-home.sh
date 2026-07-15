#!/system/bin/sh
# Ensure the curated GMS/Chrome set is enabled for the primary user, then make
# Fossify the sole HOME (Launcher3 disabled) so no home-picker chooser appears.
TARGET=org.fossify.home/org.fossify.home.activities.MainActivity
GMS_APK=/system/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk

# Google's Play emulator seeds GMS Core in userdata as a /data/app package with
# its install-time Chimera splits. Our portable overlay deliberately carves only
# product/system_ext, leaving just the GMS container APK; as a system app alone,
# it finds no usable modules. Register the identical signed container APK once
# as a Package Manager update in /data/app so FileApkMgr stages its embedded
# modules. The /data bind persists, so later VM boots take only this path check.
i=0
while [ "$i" -lt 60 ]; do
    gms_path=$(pm path com.google.android.gms 2>/dev/null | head -n 1)
    case "$gms_path" in
        package:/data/app/*) break ;;
        package:*)
            pm install -r -d --force-sdk "$GMS_APK" >/dev/null 2>&1 || \
                log -t cocoon-gms "failed to stage GMS Core in /data/app"
            break
            ;;
    esac
    i=$((i + 1))
    sleep 2
done

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
        org.fossify.home/*)
            # Fossify is the resolved HOME; disable Launcher3 so it is the only
            # HOME app and Android never shows the "Select Home app" chooser.
            pm disable-user --user 0 com.android.launcher3 >/dev/null 2>&1 || true
            exit 0
            ;;
    esac
    i=$((i + 1))
    sleep 2
done

exit 0
