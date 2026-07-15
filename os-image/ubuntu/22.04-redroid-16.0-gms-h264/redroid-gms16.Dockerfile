# ReDroid 16 (API 36) + native GMS/Play Store/Chrome, dual-arch.
#   amd64: x86_64 GMS + libndk_translation (ARM->x86 app translation).
#   arm64: native arm64 GMS, no translator (ARM apps run natively).
# Select the matching Google Play system-image overlay with
#   --build-arg GAPPS_TAR=gapps16-x86_64.tar   (default, amd64)
#   --build-arg GAPPS_TAR=gapps16-arm64.tar    (arm64)
# and build per-arch with `docker buildx build --platform linux/<arch>`.
FROM redroid/redroid:16.0.0_64only-latest@sha256:7b1e389bd15f37af3bcd06138f5b2ffa7cfba4332bd5ef54c53e99c2f160a15b AS redroid_base

# Static multi-arch BusyBox lets the bake wrapper run before Android /init has
# mounted APEX (at that point /system/bin/sh's linker target does not exist).
FROM busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94 AS bake_tools

FROM --platform=$BUILDPLATFORM ubuntu:24.04 AS overlay

ENV DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG GAPPS_TAR=gapps16-x86_64.tar
ARG NDK_TRANSLATION_COMMIT=68734c52556d3d7a6db34c603dd9276915c29f2f
ARG NDK_TRANSLATION_MD5=0b2207c490fcb400aa5c87fcf0d52d38
ARG FOSSIFY_URL=https://github.com/FossifyOrg/Launcher/releases/download/1.10.0/launcher-16-foss-release.apk
ARG FOSSIFY_SHA256=a603d3d510482feafd73d52a93a1ea9baefd2ca0aae329a14cbf0e21f43638e3

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl unzip && \
    rm -rf /var/lib/apt/lists/*

ADD ${GAPPS_TAR} /output/

# libndk (ARM->x86 translation) only on amd64; arm64 runs ARM apps natively.
RUN if [ "$TARGETARCH" = "amd64" ]; then \
      curl -fsSL \
        "https://github.com/supremegamers/vendor_google_proprietary_ndk_translation-prebuilt/archive/${NDK_TRANSLATION_COMMIT}.zip" \
        -o /tmp/ndk.zip && \
      echo "${NDK_TRANSLATION_MD5}  /tmp/ndk.zip" | md5sum -c - && \
      unzip -q /tmp/ndk.zip -d /tmp/ndk && \
      cp -a /tmp/ndk/*/prebuilts/. /output/system/ && \
      test -f /output/system/lib64/libndk_translation.so && \
      test -f /output/system/etc/init/ndk_translation.rc && \
      rm -rf /tmp/ndk.zip /tmp/ndk; \
    fi

RUN mkdir -p /output/system/app/FossifyLauncher && \
    curl -fsSL "$FOSSIFY_URL" -o /output/system/app/FossifyLauncher/FossifyLauncher.apk && \
    echo "${FOSSIFY_SHA256}  /output/system/app/FossifyLauncher/FossifyLauncher.apk" | sha256sum -c -

COPY --from=redroid_base /system/build.prop /tmp/system-build.prop
COPY --from=redroid_base /vendor/build.prop /tmp/vendor-build.prop
# amd64: point the Dalvik native bridge at libndk_translation so ARM apps run
# under x86. arm64: no bridge — leave ReDroid's build.prop untouched.
RUN if [ "$TARGETARCH" = "amd64" ]; then \
      mkdir -p /output/vendor && \
      sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' \
        /tmp/system-build.prop > /output/system/build.prop && \
      printf 'ro.ndk_translation.version=0.2.3\n' >> /output/system/build.prop && \
      sed 's|^ro.dalvik.vm.native.bridge=.*|ro.dalvik.vm.native.bridge=libndk_translation.so|' \
        /tmp/vendor-build.prop > /output/vendor/build.prop && \
      grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/system/build.prop && \
      grep -q '^ro.dalvik.vm.native.bridge=libndk_translation.so$' /output/vendor/build.prop; \
    fi && \
    test -f /output/system/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk && \
    test -f /output/system/product/priv-app/Phonesky/Phonesky.apk && \
    test -f /output/system/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk && \
    test -f /output/system/product/app/Chrome/Chrome.apk && \
    test -f /output/system/product/app/TrichromeLibrary/TrichromeLibrary.apk

COPY fossify-home.rc /output/system/etc/init/fossify-home.rc
COPY fossify-home.sh /output/system/etc/fossify-home.sh
COPY cocoon-bake-init.sh /output/cocoon-bake-init
COPY cocoon-bake-once /output/.cocoon-bake-once
COPY --from=bake_tools /bin/busybox /output/busybox
RUN chmod 0755 /output/cocoon-bake-init

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
    test -f /redroid/system/product/app/TrichromeLibrary/TrichromeLibrary.apk && \
    test -x /redroid/cocoon-bake-init && \
    test -x /redroid/busybox && \
    test -f /redroid/.cocoon-bake-once

# Repack the merged filesystem as one physical layer. This is required for the
# VM's vfs image store: every additional image layer is a full deep copy. The
# single layer also avoids retaining the source image plus a GApps delta.
FROM scratch
COPY --from=cleaned /redroid/ /
WORKDIR /
LABEL io.buildah.version="1.40.1"
ENTRYPOINT ["/busybox", "sh", "/cocoon-bake-init", "qemu=1", "androidboot.hardware=redroid"]
