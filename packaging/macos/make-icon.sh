#!/bin/sh
# Regenerates packaging/macos/basement.icns from packaging/macos/icon.svg.
#
#   packaging/macos/make-icon.sh
#
# icon.svg is the product favicon, copied verbatim from the basement website
# (basement-site/favicon.svg): a rounded square in the brand green with the
# brand orange "b." set in Arial Rounded MT Bold. Nothing here invents
# artwork; the only change is geometry, not design.
#
# macOS app icons are drawn on a 1024x1024 canvas in which the artwork itself
# occupies the centre 824x824 and the remaining margin is transparent, so the
# system can add its own shadow and so icons line up with every other icon in
# the Dock. The favicon is already a rounded square whose corner radius is
# 14/64 = 21.9% of its side, which is within a pixel of the radius macOS uses
# at that size, so the mark is scaled to 824 and centred with no reshaping.
#
# The result is committed to the repository. The release build
# (packaging/build-macos-installer.sh) only copies it, so cutting a release
# needs neither an SVG renderer nor ImageMagick — only this script does, and
# only when the brand mark itself changes.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
svg="$here/icon.svg"
icns="$here/basement.icns"

[ -f "$svg" ] || { echo "make-icon: missing $svg" >&2; exit 1; }

command -v magick >/dev/null 2>&1 || {
  echo "make-icon: needs ImageMagick (brew install imagemagick)" >&2
  exit 1
}
command -v iconutil >/dev/null 2>&1 || {
  echo "make-icon: needs iconutil (macOS command line tools)" >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Rasterise the SVG. ImageMagick's built-in SVG renderer does not lay out
# text, so prefer rsvg-convert, then headless Chrome, then a stern message:
# a silently mis-rendered icon is worse than no icon.
render() { # render <size> <out.png>
  size="$1"
  out="$2"
  if command -v rsvg-convert >/dev/null 2>&1; then
    rsvg-convert -w "$size" -h "$size" -o "$out" "$svg"
    return
  fi
  chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
  if [ -x "$chrome" ]; then
    cat > "$work/shot.html" <<HTML
<style>html,body{margin:0;padding:0;background:transparent}
svg{display:block;width:${size}px;height:${size}px}</style>
HTML
    cat "$svg" >> "$work/shot.html"
    "$chrome" --headless --disable-gpu --hide-scrollbars \
      --default-background-color=00000000 \
      --window-size="$size,$size" \
      --screenshot="$out" "$work/shot.html" >/dev/null 2>&1
    [ -s "$out" ] && return
  fi
  echo "make-icon: no SVG renderer that lays out text (install librsvg: brew install librsvg)" >&2
  exit 1
}

# 824 of 1024 is the macOS icon grid for a rounded-square mark.
render 824 "$work/mark.png"
magick "$work/mark.png" -background none -gravity center -extent 1024x1024 \
  "$work/icon-1024.png"

set -- 16 32 64 128 256 512 1024
iconset="$work/basement.iconset"
mkdir -p "$iconset"
for px in "$@"; do
  magick "$work/icon-1024.png" -filter Lanczos -resize "${px}x${px}" \
    -strip "PNG32:$work/px-$px.png"
done
# iconutil's expected names: each size at 1x, and the double of it at 2x.
cp "$work/px-16.png"   "$iconset/icon_16x16.png"
cp "$work/px-32.png"   "$iconset/icon_16x16@2x.png"
cp "$work/px-32.png"   "$iconset/icon_32x32.png"
cp "$work/px-64.png"   "$iconset/icon_32x32@2x.png"
cp "$work/px-128.png"  "$iconset/icon_128x128.png"
cp "$work/px-256.png"  "$iconset/icon_128x128@2x.png"
cp "$work/px-256.png"  "$iconset/icon_256x256.png"
cp "$work/px-512.png"  "$iconset/icon_256x256@2x.png"
cp "$work/px-512.png"  "$iconset/icon_512x512.png"
cp "$work/px-1024.png" "$iconset/icon_512x512@2x.png"

iconutil --convert icns --output "$icns" "$iconset"
echo "==> wrote $icns"
