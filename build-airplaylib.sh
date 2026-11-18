#!/usr/bin/env bash
# SPDX-License-Identifier: LGPL-3.0-or-later
#
# Builds airplaylib.aar (the AirPlay library used by AirBridge) from
#   - doubletake (LGPL-3.0, folder doubletake/) and
#   - the AirBridge extension in airbridge/ (LGPL-3.0).
# Dependency versions are pinned in pinned/go.mod and pinned/go.sum, so the
# same sources always produce the same library.
#
# Requirements: Go >= 1.25, Android SDK + NDK, Java 17, gomobile:
#   go install golang.org/x/mobile/cmd/gomobile@$(awk '/golang.org\/x\/mobile/{print $2; exit}' pinned/go.mod)
#   gomobile init
#   export ANDROID_HOME=~/Library/Android/sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<version>
#
# Usage: ./build-airplaylib.sh [output.aar]
#   UPDATE_DEPS=1 ./build-airplaylib.sh   re-resolves dependencies and rewrites pinned/
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/doubletake"
DEFAULT_OUT="$HERE/airplaylib.aar"
[ -d "$HERE/../app/libs" ] && DEFAULT_OUT="$(cd "$HERE/../app/libs" && pwd)/airplaylib.aar"
OUT="${1:-$DEFAULT_OUT}"
TARGETS="${AIRPLAYLIB_TARGETS:-android/arm64}"

[ -f "$SRC/go.mod" ] || { echo "doubletake sources missing in $SRC" >&2; exit 1; }

# A fixed build path keeps the result byte-for-byte reproducible (the path ends up in the binary).
WORK="/tmp/airplaylib-build"
rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

cp -R "$SRC" "$WORK/doubletake"
rm -rf "$WORK/doubletake/.git"
cp "$HERE/airbridge/external_source.go" "$WORK/doubletake/internal/airplay/zz_airbridge_external_source.go"
mkdir -p "$WORK/doubletake/abcore"
cp "$HERE/airbridge/abcore/abcore.go" "$WORK/doubletake/abcore/abcore.go"

cd "$WORK/doubletake"
if [ "${UPDATE_DEPS:-0}" = 1 ] || [ ! -f "$HERE/pinned/go.mod" ]; then
  go mod tidy
  # gomobile needs golang.org/x/mobile recorded as a tool dependency of the module.
  go get -tool golang.org/x/mobile/cmd/gobind@latest
  mkdir -p "$HERE/pinned"
  cp go.mod go.sum "$HERE/pinned/"
  echo "pinned dependency versions written to $HERE/pinned"
else
  cp "$HERE/pinned/go.mod" "$HERE/pinned/go.sum" .
  go mod download
fi
go vet ./abcore ./internal/airplay

gomobile bind \
  -target="$TARGETS" \
  -androidapi 26 \
  -javapkg io.github.lionfari.airplay \
  -trimpath -ldflags "-s -w" \
  -o "$OUT" \
  ./abcore

rm -f "${OUT%.aar}-sources.jar"
echo "built $OUT"
