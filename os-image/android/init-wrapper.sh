#!/sbin/busybox sh
# /sbin/init wrapper: execs Android /init with androidboot.* args.
#
# Why this exists:
# - initramfs-tools switch_root defaults to /sbin/init (or kernel init= param)
# - Android rootfs only has /init (no /sbin/init)
# - androidboot.* params must be in /init's argv (/proc/self/cmdline)
#   because redroid's init reads them from there
#
# Key params:
# - use_memfd=1: use memfd when ashmem is unavailable on modern kernels
# - selinux=permissive: required for redroid in VM mode
# - redroid_gpu_mode=guest: force software-render path in VM direct-boot mode
# - use_redroid_c2/use_dmabufheaps: prefer Codec2 path in ashmem-less kernels
# - use_redroid_omx=0: avoid unstable OMX path when gralloc.redroid requires ashmem
#
# Uses busybox (static) as interpreter — no dependency on Android's
# /system/bin/sh or linker64 at this early boot stage.
#
# Requires 64-only Redroid images (e.g. redroid:14.0.0_64only-latest).

# Relocate product/system_ext apps to /system/{app,priv-app}/ (system namespace).
#
# In bare VM, everything is on a single overlayfs — Android's libnativeloader
# can't detect partition boundaries (same st_dev), so sandboxed processes get
# "product-clns-N" classloader namespaces with restricted permitted_paths,
# blocking dlopen of native libs (e.g. WebView's libwebviewchromium.so).
# Docker doesn't hit this because its overlay layers create distinct mount points.
#
# Moving apps to /system/app/ puts them in the "system" namespace which has
# full lib access. Runs before Android init scans packages; COW layer absorbs writes.
# Can't do this in Dockerfile RUN — Android rootfs has no standard Linux linker.
BB=/sbin/busybox
SDK=$($BB grep '^ro.build.version.sdk=' /system/build.prop 2>/dev/null | $BB cut -d= -f2)

for d in /system/product/app/*/; do
    [ -d "$d" ] || continue
    name=$($BB basename "$d")
    [ -e "/system/app/$name" ] || $BB mv "$d" /system/app/
done
for d in /system/product/priv-app/*/; do
    [ -d "$d" ] || continue
    name=$($BB basename "$d")
    [ -e "/system/priv-app/$name" ] || $BB mv "$d" /system/priv-app/
done
for d in /system/system_ext/app/*/; do
    [ -d "$d" ] || continue
    name=$($BB basename "$d")
    [ -e "/system/app/$name" ] || $BB mv "$d" /system/app/
done
for d in /system/system_ext/priv-app/*/; do
    [ -d "$d" ] || continue
    name=$($BB basename "$d")
    [ -e "/system/priv-app/$name" ] || $BB mv "$d" /system/priv-app/
done

# Fix native lib symlinks pointing to product/system_ext paths.
# After moving apps to /system/app/, their lib/ dirs may contain symlinks
# like libjni_latinime.so -> /system/product/lib64/libjni_latinime.so.
# The system namespace can't dlopen from product paths, so replace symlinks
# with copies of the actual files.
for link in $($BB find /system/app /system/priv-app -type l -name '*.so' 2>/dev/null); do
    target=$($BB readlink "$link")
    case "$target" in
        /system/product/*|/system/system_ext/*)
            [ -f "$target" ] && $BB cp "$target" "${link}.tmp" && $BB mv "${link}.tmp" "$link"
            ;;
    esac
done

# Disable simulated Bluetooth — no real BT hardware in VM.
# Three layers disabled to fully prevent the crash loop:
# 1. APEX: rename com.android.btservices apex so apexd won't activate it
#    (SDK < 35 only — Android 15+ moved BT framework classes into the APEX;
#    disabling it causes SystemServiceRegistry NoClassDefFoundError)
# 2. HAL binary: prevents "Invalid address" abort from vendor HAL
# 3. VINTF manifest: prevents system_server from discovering BT HAL via HIDL
# 4. config.disable_bluetooth (in cocoon-disable-bt.rc): framework-level disable
if [ "${SDK:-0}" -lt 35 ] 2>/dev/null; then
    for f in $($BB find /system/apex -name '*btservices*' -o -name '*bluetooth*' 2>/dev/null); do
        $BB mv "$f" "${f}.disabled" 2>/dev/null
    done
fi
$BB mv /vendor/bin/hw/android.hardware.bluetooth@1.1-service.sim \
      /vendor/bin/hw/android.hardware.bluetooth@1.1-service.sim.disabled 2>/dev/null
for f in $($BB find /vendor/etc/vintf -name '*bluetooth*' 2>/dev/null); do
    $BB mv "$f" "${f}.disabled" 2>/dev/null
done

# Refresh rate: redroid guest mode leaves the display HAL at its 15fps default,
# which feels laggy even though the software renderer draws a frame in ~1ms and
# host CPU stays under 10%. Default to 60fps (measured ~4x smoother scroll);
# override via kernel cmdline redroid.fps=N (e.g. cocoon COCOON_REDROID_FPS).
REDROID_FPS=60
for _x in $($BB cat /proc/cmdline); do
    case "$_x" in redroid.fps=*) REDROID_FPS="${_x#redroid.fps=}" ;; esac
done

exec /init \
    qemu=1 \
    androidboot.hardware=redroid \
    androidboot.use_memfd=1 \
    androidboot.use_redroid_c2=1 \
    androidboot.use_dmabufheaps=1 \
    androidboot.use_redroid_omx=0 \
    androidboot.selinux=permissive \
    androidboot.redroid_gpu_mode=guest \
    androidboot.redroid_width=720 \
    androidboot.redroid_height=1280 \
    androidboot.redroid_dpi=240 \
    androidboot.redroid_fps=$REDROID_FPS
