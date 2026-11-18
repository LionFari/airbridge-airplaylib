# airbridge-airplaylib

The AirPlay library used by the Android app **AirBridge**, published under the
**GNU Lesser General Public License v3.0** (see `COPYING.LESSER` and `COPYING`).

AirBridge itself is proprietary. It uses this library as a separate, replaceable
component: the native library `libgojni.so` (plus its small Java binding in the
package `io.github.lionfari.airplay`) inside the APK.

## Contents

| Path | What it is |
| --- | --- |
| `doubletake/` | [doubletake](https://github.com/omarroth/doubletake) by Omar Roth, LGPL-3.0, unmodified copy of commit `568a06ed32afa9fafdf229a798db0786f4d020f2` |
| `airbridge/external_source.go` | AirBridge extension of doubletake: feeds video/audio encoded by the app into doubletake's streaming code (copied into `internal/airplay` at build time) |
| `airbridge/abcore/abcore.go` | The small API the app calls (bound for Android with gomobile) |
| `pinned/go.mod`, `pinned/go.sum` | Exact dependency versions used for the published app |
| `build-airplaylib.sh` | Builds `airplaylib.aar` from the above |

## Building

Requirements: Go 1.26 or newer, Java 17, Android SDK with NDK, and gomobile in
the version pinned in `pinned/go.mod`:

```bash
go install golang.org/x/mobile/cmd/gomobile@v0.0.0-20261117202452-47699113848a
gomobile init
export ANDROID_HOME=~/Library/Android/sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/27.0.12077973
./build-airplaylib.sh airplaylib.aar
```

## Replacing the library in AirBridge

1. Change the sources as you like and build `airplaylib.aar`.
2. Take `jni/arm64-v8a/libgojni.so` out of the `.aar` (it is a zip file).
3. Replace `lib/arm64-v8a/libgojni.so` in the AirBridge APK with it, then
   align and sign the APK with your own key (`zipalign`, `apksigner`) and install it.

Keep the Java API of `abcore` compatible, since the app calls it.

## Contact

Questions about this library: open an issue in this repository.

## Reproducible build

The build is byte-for-byte reproducible. Built from this repository with the
pinned versions, `jni/arm64-v8a/libgojni.so` has the SHA-256

```
d147173dfed0ec75f2e3a12768d89d7292ddd4fa8abbbb8c622a2e6787bad203
```

which is the library shipped in AirBridge from version 0.1.10 on.
