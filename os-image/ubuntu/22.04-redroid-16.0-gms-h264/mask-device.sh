#!/system/bin/sh
# Rewrite the redroid/emulator giveaway properties to a real device (Pixel 8) via
# Magisk's resetprop, so anti-emulator apps that only inspect properties do not
# bail on launch. Runs as root from mask-device.rc on boot_completed; build.prop
# cannot change these (ro.hardware comes from androidboot, the plain ro.product.*
# are set with precedence, native.bridge/abilist are runtime).
#
# This does NOT defeat SELinux-state (redroid runs SELinux disabled) or
# Play-Integrity hardware attestation — apps enforcing those still detect the
# container. It only clears the property-level tells.
MG=/system/bin/magisk_rp
RP="$MG resetprop -n"
FP="google/shiba/shiba:16/AP41.240925.009/12345678:user/release-keys"

for suf in "" .system .vendor .product .odm .system_ext; do
    $RP "ro.product${suf}.model" "Pixel 8"
    $RP "ro.product${suf}.brand" google
    $RP "ro.product${suf}.manufacturer" Google
    $RP "ro.product${suf}.device" shiba
    $RP "ro.product${suf}.name" shiba
done
for k in ro.build.fingerprint ro.system.build.fingerprint ro.vendor.build.fingerprint \
         ro.product.build.fingerprint ro.bootimage.build.fingerprint; do
    $RP "$k" "$FP"
done
$RP ro.build.type user
$RP ro.build.tags release-keys
$RP ro.build.flavor shiba-user
$RP ro.hardware shiba
$RP ro.boot.hardware shiba
$RP ro.hardware.egl mali
$RP ro.dalvik.vm.native.bridge 0
$RP ro.product.cpu.abilist arm64-v8a
$RP ro.product.cpu.abilist64 arm64-v8a
$RP ro.boot.verifiedbootstate green
$RP ro.boot.flash.locked 1
$RP ro.boot.veritymode enforcing
