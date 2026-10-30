#!/bin/bash
set -euo pipefail
app="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM_NAME=macosx bash "$app/Scripts/build-vision.sh"
lib="$app/Build/VisualRecognition/macosx/install"
xcrun clang++ -std=c++17 -O2 -mmacosx-version-min=26.0 -I"$lib/include/opencv4" \
 "$app/Sources/App/SiteLocalization.cpp" "$app/Tests-Native/SiteLocalizationTests.cpp" \
 -L"$lib/lib" -lopencv_calib3d -lopencv_features2d -lopencv_flann -lopencv_imgproc -lopencv_core "$lib/lib/opencv4/3rdparty/libtegra_hal.a" -lz \
 -o "$app/Build/VisualRecognition/test-localization"
"$app/Build/VisualRecognition/test-localization"
