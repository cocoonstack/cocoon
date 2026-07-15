#!/system/bin/sh
# Rewrite the redroid/emulator IDENTITY properties to a real device (Pixel 8) via
# Magisk's resetprop, so anti-emulator apps that only inspect properties do not
# bail on launch. Runs as root from mask-device.rc on boot_completed; build.prop
# cannot change these (ro.hardware comes from androidboot, the plain ro.product.*
# are set with precedence).
#
# Only cosmetic identity props are touched. Runtime-consumed props are left as-is
# on purpose: rewriting ro.product.cpu.abilist / ro.dalvik.vm.native.bridge would
# break libndk ARM translation if zygote restarts, and ro.debuggable=0 tears down
# the scrcpy/remoteview adb tunnel. Those, plus ro.bionic.arch and dalvik.vm.isa.*
# (which report x86_64), remain visible — the x86 runtime is not maskable and,
# like SELinux-disabled and no-TEE Play Integrity, still gives the container away.
RP="/system/bin/resetprop -n"
DEL="/system/bin/resetprop --delete"
FP="google/shiba/shiba:16/AP41.240925.009/12345678:user/release-keys"

# redroid stamps identity and build descriptors on EVERY partition
# (system/vendor/product/odm/system_ext/vendor_dlkm), so a naive getprop scan of
# any one partition must not reveal redroid/userdebug/test-keys/eng.root.
for suf in "" .system .vendor .product .odm .system_ext .vendor_dlkm; do
    $RP "ro.product${suf}.model" "Pixel 8"
    $RP "ro.product${suf}.brand" google
    $RP "ro.product${suf}.manufacturer" Google
    $RP "ro.product${suf}.device" shiba
    $RP "ro.product${suf}.name" shiba
    $RP "ro${suf}.build.fingerprint" "$FP"
    $RP "ro${suf}.build.type" user
    $RP "ro${suf}.build.tags" release-keys
    $RP "ro${suf}.build.id" AP41.240925.009
    $RP "ro${suf}.build.version.incremental" 12345678
done
$RP ro.bootimage.build.fingerprint "$FP"
$RP ro.build.flavor shiba-user
$RP ro.build.description "shiba-user 16 AP41.240925.009 12345678 release-keys"
$RP ro.build.display.id AP41.240925.009
$RP ro.build.product shiba
$RP ro.hardware shiba
$RP ro.boot.hardware shiba
$RP ro.hardware.egl mali
$RP ro.hardware.vulkan mali
$RP ro.product.board zuma
$RP ro.board.platform zuma
$RP ro.bootloader shiba-1.4-11893630
$RP ro.build.user android-build
$RP ro.build.host abfarm-release.google.com
$RP ro.boot.verifiedbootstate green
$RP ro.boot.flash.locked 1
$RP ro.boot.veritymode enforcing

# Drop the redroid-internal boot hints (their names literally contain "redroid")
# and the redroid gralloc tag; they are consumed at boot and cosmetic afterwards.
for p in ro.boot.redroid_dpi ro.boot.redroid_fps ro.boot.redroid_gpu_mode \
         ro.boot.redroid_height ro.boot.redroid_width ro.boot.redroid_net_dns1 \
         ro.boot.redroid_net_dns2 ro.boot.redroid_net_ndns ro.boot.use_redroid_c2 \
         ro.hardware.gralloc; do
    $DEL "$p"
done
