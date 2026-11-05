#!/usr/bin/env bash
set -euo pipefail
app_root="$(cd "$(dirname "$0")/.." && pwd)"
repo_root="$(cd "$app_root/../.." && pwd)"
ndk="${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-}}"
if [[ -z "$ndk" ]]; then
  sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/Library/Android/sdk}}"
  ndk="$sdk/ndk/29.0.14206865"
  if [[ ! -d "$ndk" && -d /opt/homebrew/share/android-ndk ]]; then ndk=/opt/homebrew/share/android-ndk; fi
fi
case "$(uname -s)" in Darwin) host=darwin-x86_64 ;; Linux) host=linux-x86_64 ;; *) echo 'Build the Go library on macOS or Linux.' >&2; exit 1 ;; esac
toolchain="$ndk/toolchains/llvm/prebuilt/$host/bin"
if [[ ! -d "$toolchain" ]]; then echo 'Set ANDROID_NDK_HOME to Android NDK r29 or newer.' >&2; exit 1; fi
IFS=',' read -ra abis <<< "${WENDY_ANDROID_ABIS:-arm64-v8a,x86_64}"
cd "$repo_root"
for abi in "${abis[@]}"; do
  case "$abi" in
    arm64-v8a) arch=arm64; target=aarch64-linux-android ;;
    x86_64) arch=amd64; target=x86_64-linux-android ;;
    *) echo "Unsupported ABI: $abi" >&2; exit 1 ;;
  esac
  output="$app_root/app/src/main/jniLibs/$abi"
  mkdir -p "$output"
  GOOS=android GOARCH="$arch" CGO_ENABLED=1 CC="$toolchain/${target}29-clang" \
    go build -trimpath -buildmode=c-shared -ldflags='-s -w -checklinkname=0 -extldflags=-Wl,-z,max-page-size=16384' -o "$output/libwendy.so" ./go/android/bridge
  rm -f "$output/libwendy.h"
done
