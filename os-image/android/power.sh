#!/system/bin/sh
# Watches the ACPI power button and turns a press into an Android shutdown.
for ev in /sys/class/input/event*; do
    [ "$(cat "$ev/device/name" 2>/dev/null)" = "Power Button" ] || continue
    node=/dev/input/${ev##*/}
    dev=$(cat "$ev/dev")
    [ -c "$node" ] || mknod "$node" c "${dev%%:*}" "${dev##*:}"
    getevent -ql "$node" | while read -r _ code value; do
        [ "$code" = KEY_POWER ] && [ "$value" = DOWN ] && setprop sys.powerctl shutdown
    done
done
