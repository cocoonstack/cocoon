#!/bin/bash
# prepare-gapps16.sh — fetch Google's API-36 Play Store system image and carve the
# GApps overlay ReDroid needs (GMS/Play Store/Chrome/GSF), emitting gapps16-<abi>.tar.
#
# Chain: sys-img zip -> <abi>/system.img (a GPT disk: vbmeta + super) -> extract the
# 'super' partition -> lpunpack -> product.img + system_ext.img (EROFS) ->
# prepare-gapps16-inner.sh (reads /cache/partitions/*.img). GApps are Google
# proprietary: only the raw Google zip is cached, never the derived tar.
#
#   ARCH   amd64|arm64  (default amd64)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARCH="${ARCH:-amd64}"
case "$ARCH" in
  amd64)
    ABI=x86_64
    DEFAULT_ZIP=x86_64-36_r07.zip
    DEFAULT_SHA1=16fa3c441d29fde6c9eea2f766eecf77032d68b4
    ;;
  arm64)
    ABI=arm64-v8a
    DEFAULT_ZIP=arm64-v8a-36_r07.zip
    DEFAULT_SHA1=5b91ef80a5eb60d7b7167528db259c2a12d20e9c
    ;;
  *) echo "unsupported ARCH: $ARCH" >&2; exit 1 ;;
esac
OUT="gapps16-${ABI}.tar"
REPO="https://dl.google.com/android/repository"
LPUNPACK_URL="https://raw.githubusercontent.com/unix3dgforce/lpunpack/master/lpunpack.py"
LPUNPACK_SHA256="600d1cab2fc7de5127fb287f4e2791718db54efb1102d0f365db0d3ec1ccf62e"
CACHE="$HERE/.cache-gapps16"
mkdir -p "$CACHE"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ZIP="${GAPPS_ZIP:-$DEFAULT_ZIP}"
SHA1="${GAPPS_SHA1:-$DEFAULT_SHA1}"
[[ "$SHA1" =~ ^[0-9a-f]{40}$ ]] || { echo "invalid GApps SHA-1: $SHA1" >&2; exit 1; }
echo ">> pinned ${ABI} API-36 Play Store image: $ZIP sha1=$SHA1"

ZIPPATH="$CACHE/$ZIP"
if ! { [ -f "$ZIPPATH" ] && echo "$SHA1  $ZIPPATH" | sha1sum -c - >/dev/null 2>&1; }; then
  echo ">> downloading $ZIP (~1.9 GB)"
  curl -fL "$REPO/sys-img/google_apis_playstore/$ZIP" -o "$ZIPPATH"
  echo "$SHA1  $ZIPPATH" | sha1sum -c -
fi

echo ">> unzip ${ABI}/system.img"
unzip -o -q "$ZIPPATH" -d "$WORK" "${ABI}/system.img"
SYS="$WORK/${ABI}/system.img"

echo ">> locate + extract the 'super' partition from the GPT"
read -r START SIZE < <(python3 - "$SYS" <<'PY'
import struct, sys
f = open(sys.argv[1], 'rb')
f.seek(512)
hdr = f.read(92)
assert hdr[:8] == b'EFI PART', "system.img is not a GPT disk"
pe_lba, num, esz = struct.unpack('<QII', hdr[72:88])
f.seek(pe_lba * 512)
for _ in range(num):
    e = f.read(esz)
    first, last = struct.unpack('<QQ', e[32:48])
    name = e[56:128].decode('utf-16-le').rstrip('\x00')
    if name == 'super':
        print(first * 512, (last - first + 1) * 512)
        break
PY
)
[ -n "${SIZE:-}" ] || { echo "no 'super' partition in system.img" >&2; exit 1; }
python3 - "$SYS" "$WORK/super.img" "$START" "$SIZE" <<'PY'
import shutil, sys

src, dst, start, size = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
with open(src, 'rb') as source, open(dst, 'wb') as target:
    source.seek(start)
    remaining = size
    while remaining:
        chunk = source.read(min(8 * 1024 * 1024, remaining))
        if not chunk:
            raise EOFError("system.img ended before the super partition was complete")
        target.write(chunk)
        remaining -= len(chunk)
PY

echo ">> lpunpack super -> product.img + system_ext.img"
curl -fsSL "$LPUNPACK_URL" -o "$WORK/lpunpack.py"
echo "$LPUNPACK_SHA256  $WORK/lpunpack.py" | sha256sum -c -
mkdir -p "$WORK/lp"
python3 "$WORK/lpunpack.py" --partition=product,system_ext "$WORK/super.img" "$WORK/lp" \
  || python3 "$WORK/lpunpack.py" "$WORK/super.img" "$WORK/lp"
test -f "$WORK/lp/product.img"
test -f "$WORK/lp/system_ext.img"

echo ">> carve GApps overlay via prepare-gapps16-inner.sh"
mkdir -p "$WORK/cache/partitions"
cp "$WORK/lp/product.img" "$WORK/lp/system_ext.img" "$WORK/cache/partitions/"
docker run --rm \
  -v "$WORK/cache":/cache:ro \
  -v "$HERE":/out \
  -e OUT_NAME="$OUT" \
  ubuntu:24.04 bash /out/prepare-gapps16-inner.sh
test -f "$HERE/$OUT"
echo ">> $OUT $(du -h "$HERE/$OUT" | cut -f1)"
