#!/usr/bin/env bash
# Regenerate every PNG icon under web/public from one geometry.
#
# The design source is web/public/icons/icon.svg: a #2563eb tile with
# rx=22 and a white terminal-prompt glyph (chevron + underscore) on a
# 100x100 grid. ImageMagick's built-in SVG renderer drops stroked paths
# and the librsvg delegate is not a repo dependency, so this script
# redraws the same geometry with MVG primitives at 10x and downsamples.
# Keep the coordinates below in step with icon.svg.
#
# Outputs (all 8-bit PNG, metadata stripped):
#   icons/icon-{192,512}.png           manifest "any": rounded tile, transparent corners
#   icons/icon-maskable-{192,512}.png  manifest "maskable": full bleed, glyph at 72%
#   apple-touch-icon.png               iOS Home Screen: 180x180, full bleed, OPAQUE
#
# iOS composites a transparent touch icon onto black (black corner wedges)
# and on some versions discards it for a screenshot tile, so the Apple icon
# is square-cornered and alpha-free; iOS applies its own squircle mask.
set -euo pipefail

cd "$(dirname "$0")/../public"

BG='#2563eb'
GLYPH='stroke-linecap round stroke-linejoin round polyline 300,330 520,500 300,670 line 560,670 720,670'

# render <size> <out> <tile-draw> <glyph-scale> <alpha: on|off>
# png:color-type pins truecolor (2 = RGB, 6 = RGBA) so ImageMagick never
# picks a palette encoding for the flat-colour tiles.
render() {
  local size=$1 out=$2 tile=$3 scale=$4 alpha=$5
  local ctype=6
  [ "$alpha" = off ] && ctype=2
  magick -size 1000x1000 xc:none \
    -fill "$BG" -stroke none -draw "$tile" \
    -fill none -stroke white -strokewidth 110 \
    -draw "push graphic-context translate 500,500 scale $scale,$scale translate -500,-500 $GLYPH pop graphic-context" \
    -filter Lanczos -resize "${size}x${size}" \
    -alpha "$alpha" -depth 8 -define png:color-type=$ctype -strip "$out"
  echo "wrote $out ($(magick identify -format '%wx%h %[channels]' "$out"))"
}

ROUNDED='roundrectangle 0,0 999,999 220,220'
SQUARE='rectangle 0,0 999,999'

render 192 icons/icon-192.png "$ROUNDED" 1 on
render 512 icons/icon-512.png "$ROUNDED" 1 on
render 192 icons/icon-maskable-192.png "$SQUARE" 0.72 off
render 512 icons/icon-maskable-512.png "$SQUARE" 0.72 off
render 180 apple-touch-icon.png "$SQUARE" 1 off
