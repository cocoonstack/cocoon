# cmgs/redroid-gms:15.0 — redroid 15 + GMS + libndk ARM translation + Lawnchair,
# as a plain docker image for native redroid usage (keeps redroid's entrypoint).
# All payloads pinned by checksum.
#
# GMS placement is native: redroid's /product and /system_ext are symlinks into
# /system, so GmsCore/Phonesky under /system/product/priv-app register with no
# init-wrapper (redroid ships its own product priv-apps there too). Lawnchair +
# Lawnicons go in /system/app and lawnchair-home.rc makes Lawnchair the default
# HOME on boot_completed.
FROM ubuntu:24.04 AS gapps

ENV DEBIAN_FRONTEND=noninteractive
ARG NDK_TRANSLATION_COMMIT=68734c52556d3d7a6db34c603dd9276915c29f2f
ARG NDK_TRANSLATION_MD5=0b2207c490fcb400aa5c87fcf0d52d38
ARG LITEGAPPS_URL="https://downloads.sourceforge.net/project/litegapps/litegapps/x86_64/35/lite/2024-10-27/LiteGapps-x86_64-15.0-20241027-official.zip"
ARG LITEGAPPS_SHA256=a868e988828487fb7d5fd74b3f527493a10e915e4a7c7f0cfb2e7d306e1d58cb
ARG LAWNCHAIR_URL="https://github.com/LawnchairLauncher/lawnchair/releases/download/v15.0.0-beta3.0/Lawnchair.15.0.0.Beta.3.0.apk"
ARG LAWNCHAIR_SHA256=d4200d0985169fd79ba1bd225d653f2a2fe7b50aa07cb0d05ca64c7623f86059
ARG LAWNICONS_URL="https://github.com/LawnchairLauncher/lawnicons/releases/download/v2.18.0/Lawnicons.2.18.0.apk"
ARG LAWNICONS_SHA256=e830b37e1cd7cd66487492f4a1084ba086254b2b09541d729da0ec2169a73bfe

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl unzip xz-utils && \
    mkdir -p /output/system && \
    curl -fsSL "https://github.com/supremegamers/vendor_google_proprietary_ndk_translation-prebuilt/archive/${NDK_TRANSLATION_COMMIT}.zip" -o /tmp/ndk.zip && \
    echo "${NDK_TRANSLATION_MD5}  /tmp/ndk.zip" | md5sum -c - && \
    unzip -q /tmp/ndk.zip -d /tmp/ndk && \
    cp -a /tmp/ndk/*/prebuilts/. /output/system/ && \
    test -f /output/system/lib64/libndk_translation.so && \
    test -f /output/system/etc/init/ndk_translation.rc && \
    curl -fsSL "${LITEGAPPS_URL}" -o /tmp/lg.zip && \
    echo "${LITEGAPPS_SHA256}  /tmp/lg.zip" | sha256sum -c - && \
    unzip -q /tmp/lg.zip 'files/*' -d /tmp/lg && \
    tar -xJf /tmp/lg/files/files.tar.xz -C /tmp/lg && \
    cp -a /tmp/lg/x86_64/35/system/. /output/system/ && \
    test -f /output/system/product/priv-app/GmsCore/GmsCore.apk && \
    test -f /output/system/product/priv-app/Phonesky/Phonesky.apk && \
    cp -a /output/system/product/etc/permissions/. /output/system/etc/permissions/ && \
    cp -a /output/system/system_ext/etc/permissions/. /output/system/etc/permissions/ && \
    # Lawnchair launcher + Lawnicons icon pack as system apps.
    mkdir -p /output/system/app/Lawnchair /output/system/app/Lawnicons && \
    curl -fsSL "${LAWNCHAIR_URL}" -o /output/system/app/Lawnchair/Lawnchair.apk && \
    echo "${LAWNCHAIR_SHA256}  /output/system/app/Lawnchair/Lawnchair.apk" | sha256sum -c - && \
    curl -fsSL "${LAWNICONS_URL}" -o /output/system/app/Lawnicons/Lawnicons.apk && \
    echo "${LAWNICONS_SHA256}  /output/system/app/Lawnicons/Lawnicons.apk" | sha256sum -c - && \
    rm -rf /tmp/ndk.zip /tmp/ndk /tmp/lg.zip /tmp/lg /var/lib/apt/lists/*

# Patch native-bridge in build.prop here (redroid has no /bin/sh, so it cannot
# RUN on the final stage); the patched props are overlaid via COPY below.
COPY --from=redroid/redroid:15.0.0_64only-latest /system/build.prop /tmp/system-build.prop
COPY --from=redroid/redroid:15.0.0_64only-latest /vendor/build.prop /tmp/vendor-build.prop
RUN mkdir -p /output/vendor && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' /tmp/system-build.prop > /output/system/build.prop && \
    printf 'ro.ndk_translation.version=0.2.3\n' >> /output/system/build.prop && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' /tmp/vendor-build.prop > /output/vendor/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/system/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/vendor/build.prop

FROM redroid/redroid:15.0.0_64only-latest

COPY --from=gapps /output/system/ /system/
COPY --from=gapps /output/vendor/ /vendor/
COPY lawnchair-home.rc /system/etc/init/lawnchair-home.rc
