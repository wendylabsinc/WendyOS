#!/bin/bash
set -euo pipefail
app="$1"; platform="$2"
cache="$app/Build/VisualRecognition"
version=4.12.0
sha=44c106d5bb47efec04e531fd93008b3fcd1d27138985c5baf4eafac0e1ec9e9d
archive="$cache/opencv-$version.tar.gz"
source="$cache/opencv-$version"
output="$cache/$platform"
if [ -f "$output/.complete-$version-v1" ]; then exit 0; fi
if [ ! -d "$source" ]; then
  if [ ! -f "$archive" ]; then
    curl --fail --location --retry 3 "https://github.com/opencv/opencv/archive/refs/tags/$version.tar.gz" -o "$archive.tmp"
    mv "$archive.tmp" "$archive"
  fi
  actual=$(shasum -a 256 "$archive" | cut -d ' ' -f 1)
  if [ "$actual" != "$sha" ]; then echo "OpenCV source checksum mismatch" >&2; exit 1; fi
  tar -xzf "$archive" -C "$cache"
fi
args=(-DCMAKE_OSX_ARCHITECTURES=arm64 -DCMAKE_OSX_SYSROOT="$(xcrun --sdk "$platform" --show-sdk-path)")
case "$platform" in
 macosx) args+=(-DCMAKE_OSX_DEPLOYMENT_TARGET=26.0) ;;
 iphoneos|iphonesimulator) args+=(-DCMAKE_SYSTEM_NAME=iOS -DCMAKE_OSX_DEPLOYMENT_TARGET=26.0 -DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY) ;;
 *) echo "Unsupported recognition platform: $platform" >&2; exit 1 ;;
esac
cmake -S "$source" -B "$output" -G Ninja "${args[@]}" \
 -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$output/install" \
 -DBUILD_LIST=core,imgproc,features2d,calib3d,flann -DBUILD_SHARED_LIBS=OFF \
 -DBUILD_TESTS=OFF -DBUILD_PERF_TESTS=OFF -DBUILD_EXAMPLES=OFF -DBUILD_opencv_apps=OFF \
 -DBUILD_JAVA=OFF -DBUILD_opencv_python2=OFF -DBUILD_opencv_python3=OFF \
 -DWITH_ITT=OFF -DWITH_IPP=OFF -DWITH_OPENCL=OFF -DWITH_OPENCLAMDBLAS=OFF \
 -DWITH_LAPACK=OFF -DWITH_EIGEN=OFF -DWITH_ADE=OFF -DWITH_PROTOBUF=OFF \
 -DWITH_FFMPEG=OFF -DWITH_GSTREAMER=OFF -DWITH_AVFOUNDATION=OFF \
 -DWITH_JPEG=OFF -DWITH_PNG=OFF -DWITH_TIFF=OFF -DWITH_OPENEXR=OFF -DWITH_WEBP=OFF \
 -DCV_ENABLE_INTRINSICS=ON -DOPENCV_ENABLE_NONFREE=OFF
cmake --build "$output" --parallel 8
cmake --install "$output"
touch "$output/.complete-$version-v1"
