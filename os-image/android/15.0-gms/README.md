# Android 15 + GMS + ARM translation (`15.0-gms`)

Stock `15.0` plus Google Play services and an ARM→x86 translator, so an x86_64
host can run arm64-only APKs and log into Google Play.

`linux/amd64` only (the translator and GMS payload are x86_64).

## What it adds over `15.0`

- **`libndk_translation`** — Google's ARM→x86 binary translator (ChromeOS
  prebuilt, same source and pin as `waydroid_script`). The stock redroid image
  ships the native-bridge props but no translator library, so arm64-only APKs
  install and then crash on launch; this variant supplies the library and wires
  `ro.dalvik.vm.native.bridge` in both `system` and `vendor` build.prop.
- **GMS core** (LiteGapps lite): GmsCore, Play Store, Google Services
  Framework, and the calendar/contacts sync adapters. Chrome and other Google
  apps install natively through Play once signed in.
- `binfmt_misc` in the initramfs so the translator can register its ARM exec
  handlers.

All payloads are pinned by URL + checksum in the Dockerfile and re-verified at
build time (`test -f`/`grep -q`), so the image is reproducible.

## Capability boundary

| Works | Does not work |
|-------|---------------|
| Google Play login, Play Store installs | Apps that hook JNI or self-check for tampering |
| Ordinary arm64 apps (verified: Meituan — installs, launches, renders) | WeChat, QQ, Alipay, banking apps, most anti-cheat games |

Hardened apps crash at startup inside their own protection layer (e.g.
`libowl.so` hooking `GetMethodID`, `libaff_biz.so` in `JNI_OnLoad`): under a
native bridge the app sees a proxied `JNIEnv` whose entries point at translator
trampolines, not real ARM code, so a hook that rewrites or scans that memory
faults. This is a property of x86 binary translation, not a configuration gap —
libndk, berberis, and houdini all hit it, and there is no freely
redistributable translator that both targets Android 15 and tolerates these
apps.

**To run hardened arm64 apps, use an arm64 host with the plain `15.0` image**:
native execution, no translation, no hooking conflict.

## Licensing

`libndk_translation` is Google-proprietary (extracted from ChromeOS) and the
GMS APKs are Google's. This variant is for evaluation; redistribution terms are
Google's, not cocoon's.

## Run

```bash
cocoon vm run --name android --cpu 4 --memory 4G ghcr.io/cocoonstack/cocoon/android:15.0-gms
adb connect <vm-ip>:5555
```
