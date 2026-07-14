#!/bin/bash
# Runs inside Ubuntu with /cache containing unpacked Google partition images and
# /out pointing at this Dockerfile directory. Produces a curated ReDroid overlay.
set -euo pipefail

apt-get update -qq
apt-get install -y -qq --no-install-recommends erofs-utils gzip >/dev/null

rm -rf /tmp/google-product /tmp/google-system-ext /tmp/gapps-out
mkdir -p /tmp/google-product /tmp/google-system-ext /tmp/gapps-out/system
fsck.erofs --extract=/tmp/google-product /cache/partitions/product.img >/dev/null
fsck.erofs --extract=/tmp/google-system-ext /cache/partitions/system_ext.img >/dev/null

install_tree() {
    local src=$1 dst=$2
    mkdir -p "$(dirname "$dst")"
    cp -a "$src" "$dst"
    find "$dst" -type d -name oat -prune -exec rm -rf {} +
}

# API-36 x86_64 GMS core from Google's Play Store emulator image. Avoid emulator
# Settings/SystemUI/HAL packages; ReDroid keeps its own framework and hardware UI.
install_tree /tmp/google-product/priv-app/PrebuiltGmsCore \
    /tmp/gapps-out/system/product/priv-app/PrebuiltGmsCore
install_tree /tmp/google-product/priv-app/Phonesky \
    /tmp/gapps-out/system/product/priv-app/Phonesky
install_tree /tmp/google-product/priv-app/GoogleOneTimeInitializer \
    /tmp/gapps-out/system/product/priv-app/GoogleOneTimeInitializer
install_tree /tmp/google-product/priv-app/PartnerSetupPrebuilt \
    /tmp/gapps-out/system/product/priv-app/PartnerSetupPrebuilt
install_tree /tmp/google-system-ext/priv-app/GoogleServicesFramework \
    /tmp/gapps-out/system/system_ext/priv-app/GoogleServicesFramework
install_tree /tmp/google-system-ext/priv-app/GoogleFeedback \
    /tmp/gapps-out/system/system_ext/priv-app/GoogleFeedback

# Chrome uses TrichromeLibrary. The emulator stores the full APKs gzip-compressed
# next to tiny stubs; install the full x86_64 APKs directly for offline first boot.
mkdir -p /tmp/gapps-out/system/product/app/Chrome
gzip -dc /tmp/google-product/app/Chrome/Chrome.apk.gz \
    > /tmp/gapps-out/system/product/app/Chrome/Chrome.apk
mkdir -p /tmp/gapps-out/system/product/app/TrichromeLibrary
gzip -dc /tmp/google-product/app/TrichromeLibrary/TrichromeLibrary.apk.gz \
    > /tmp/gapps-out/system/product/app/TrichromeLibrary/TrichromeLibrary.apk
chmod 0644 /tmp/gapps-out/system/product/app/Chrome/Chrome.apk \
    /tmp/gapps-out/system/product/app/TrichromeLibrary/TrichromeLibrary.apk

mkdir -p /tmp/gapps-out/system/product/etc/permissions
for f in privapp-permissions-google-p.xml \
         privapp-permissions-sdk-google.xml \
         split-permissions-google.xml; do
    cp -a "/tmp/google-product/etc/permissions/$f" \
        /tmp/gapps-out/system/product/etc/permissions/
done

mkdir -p /tmp/gapps-out/system/system_ext/etc/permissions
cp -a /tmp/google-system-ext/etc/permissions/privapp-permissions-google-se.xml \
    /tmp/gapps-out/system/system_ext/etc/permissions/

mkdir -p /tmp/gapps-out/system/product/etc/default-permissions
cp -a /tmp/google-product/etc/default-permissions/default-permissions-sdk-google.xml \
    /tmp/gapps-out/system/product/etc/default-permissions/

mkdir -p /tmp/gapps-out/system/product/etc/sysconfig
for f in google.xml google_build.xml google-hiddenapi-package-whitelist.xml \
         google-staged-installer-whitelist.xml; do
    cp -a "/tmp/google-product/etc/sysconfig/$f" \
        /tmp/gapps-out/system/product/etc/sysconfig/
done

if [ -d /tmp/google-product/etc/security/fsverity ]; then
    mkdir -p /tmp/gapps-out/system/product/etc/security
    cp -a /tmp/google-product/etc/security/fsverity \
        /tmp/gapps-out/system/product/etc/security/
fi

tar --numeric-owner --xattrs -C /tmp/gapps-out -cf /out/gapps16-x86_64.tar .
sha256sum /out/gapps16-x86_64.tar
