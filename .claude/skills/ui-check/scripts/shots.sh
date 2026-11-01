#!/bin/bash
# Screenshots of an lt page: light/dark x desktop/phone. Usage: shots.sh <file.html|url> [outdir]
set -u
src=${1:?usage: shots.sh <report.html|url> [outdir]}
out=${2:-$(mktemp -d)}
mkdir -p "$out"
case "$src" in
  http*|file:*) url=$src ;;
  *) url="file://$(cd "$(dirname "$src")" && pwd)/$(basename "$src")" ;;
esac
chrome=""
for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" google-chrome chromium chromium-browser; do
  if [ -x "$c" ] || command -v "$c" >/dev/null 2>&1; then chrome=$c; break; fi
done
[ -n "$chrome" ] || { echo "no Chrome/Chromium found"; exit 2; }

# Headless Chrome will not lay out narrower than ~500px, so phone widths load
# the page inside a frame of the wanted width.
frame() { # frame <width> <height> -> url of a wrapper page
  local f="$out/frame-$1.html"
  printf '<!doctype html><body style="margin:0;background:#888"><iframe src="%s" width="%s" height="%s" style="border:0;display:block"></iframe></body>' "$url" "$1" "$2" >"$f"
  echo "file://$f"
}
shot() { # shot <name> <width> <height> <scheme 1=light 0=dark>
  local target=$url win=$2
  if [ "$2" -lt 500 ]; then target=$(frame "$2" "$3"); win=500; fi
  "$chrome" --headless=new --disable-gpu --hide-scrollbars --timeout=6000 \
    --blink-settings=preferredColorScheme=$4 --window-size="$win,$3" \
    --screenshot="$out/$1.png" "$target" >/dev/null 2>&1 &
  local p=$!
  ( sleep 20; kill $p 2>/dev/null ) &
  local w=$!
  wait $p 2>/dev/null; kill $w 2>/dev/null
  [ -s "$out/$1.png" ] && echo "$out/$1.png" || echo "failed: $1"
}
# preferredColorScheme: 0 = dark, 1 = light (Blink setting values).
# Phone shots are 500px wide images with the 390px page on the left.
shot light-desktop 1440 3400 1
shot dark-desktop 1440 3400 0
shot light-phone 390 4200 1
shot dark-phone 390 4200 0
shot light-top 1440 1000 1
