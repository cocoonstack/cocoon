# ReDroid 16 (API 36) + native x86_64 GMS/Play Store/Chrome + ARM translation.
# gapps16-x86_64.tar is prepared from Google's pinned API-36 Play Store system image.
FROM redroid/redroid:16.0.0_64only-latest@sha256:7b1e389bd15f37af3bcd06138f5b2ffa7cfba4332bd5ef54c53e99c2f160a15b AS redroid_base

FROM --platform=$BUILDPLATFORM ubuntu:24.04 AS overlay

ENV DEBIAN_FRONTEND=noninteractive
ARG NDK_TRANSLATION_COMMIT=68734c52556d3d7a6db34c603dd9276915c29f2f
ARG NDK_TRANSLATION_MD5=0b2207c490fcb400aa5c87fcf0d52d38
ARG FOSSIFY_URL=https://github.com/FossifyOrg/Launcher/releases/download/1.10.0/launcher-16-foss-release.apk
ARG FOSSIFY_SHA256=a603d3d510482feafd73d52a93a1ea9baefd2ca0aae329a14cbf0e21f43638e3

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl unzip && \
    rm -rf /var/lib/apt/lists/*

ADD gapps16-x86_64.tar /output/

RUN curl -fsSL \
      "https://github.com/supremegamers/vendor_google_proprietary_ndk_translation-prebuilt/archive/${NDK_TRANSLATION_COMMIT}.zip" \
      -o /tmp/ndk.zip && \
    echo "${NDK_TRANSLATION_MD5}  /tmp/ndk.zip" | md5sum -c - && \
    unzip -q /tmp/ndk.zip -d /tmp/ndk && \
    cp -a /tmp/ndk/*/prebuilts/. /output/system/ && \
    test -f /output/system/lib64/libndk_translation.so && \
    test -f /output/system/etc/init/ndk_translation.rc && \
    mkdir -p /output/system/app/FossifyLauncher && \
    curl -fsSL "$FOSSIFY_URL" -o /output/system/app/FossifyLauncher/FossifyLauncher.apk && \
    echo "${FOSSIFY_SHA256}  /output/system/app/FossifyLauncher/FossifyLauncher.apk" | sha256sum -c - && \
    rm -rf /tmp/ndk.zip /tmp/ndk

COPY --from=redroid_base /system/build.prop /tmp/system-build.prop
COPY --from=redroid_base /vendor/build.prop /tmp/vendor-build.prop
RUN mkdir -p /output/vendor && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' \
      /tmp/system-build.prop > /output/system/build.prop && \
    printf 'ro.ndk_translation.version=0.2.3\n' >> /output/system/build.prop && \
    sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' \
      /tmp/vendor-build.prop > /output/vendor/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/system/build.prop && \
    grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/vendor/build.prop && \
    test -f /output/system/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk && \
    test -f /output/system/product/priv-app/Phonesky/Phonesky.apk && \
    test -f /output/system/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk && \
    test -f /output/system/product/app/Chrome/Chrome.apk && \
    test -f /output/system/product/app/TrichromeLibrary/TrichromeLibrary.apk

COPY fossify-home.rc /output/system/etc/init/fossify-home.rc
COPY fossify-home.sh /output/system/etc/fossify-home.sh

FROM redroid_base AS merged

COPY --from=overlay /output/ /

# Remove only user-facing apps that are useless in this headless VM. Browser2
# is Chromium's WebView test shell, not the Android System WebView provider.
# Keep Launcher3 installed as a recovery HOME while Fossify is selected at boot.
FROM --platform=$BUILDPLATFORM ubuntu:24.04 AS cleaned

COPY --from=merged / /redroid/
RUN rm -rf \
      /redroid/system/product/app/Camera2 \
      /redroid/system/product/app/Browser2 && \
    test ! -e /redroid/system/product/app/Camera2 && \
    test ! -e /redroid/system/product/app/Browser2 && \
    test -f /redroid/system/product/app/webview/webview.apk && \
    test -f /redroid/system/system_ext/priv-app/Launcher3QuickStep/Launcher3QuickStep.apk && \
    test -f /redroid/system/app/FossifyLauncher/FossifyLauncher.apk && \
    test -f /redroid/system/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk && \
    test -f /redroid/system/product/priv-app/Phonesky/Phonesky.apk && \
    test -f /redroid/system/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk && \
    test -f /redroid/system/product/app/Chrome/Chrome.apk && \
    test -f /redroid/system/product/app/TrichromeLibrary/TrichromeLibrary.apk

# Repack the merged filesystem as one physical layer. This is required for the
# VM's vfs image store: every additional image layer is a full deep copy. The
# single layer also avoids retaining the source image plus a GApps delta.
FROM scratch
COPY --from=cleaned /redroid/ /
WORKDIR /
LABEL io.buildah.version="1.40.1"
ENTRYPOINT ["/init", "qemu=1", "androidboot.hardware=redroid"]
