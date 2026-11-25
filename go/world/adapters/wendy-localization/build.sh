#!/bin/bash
set -euo pipefail
if [ "$#" -ne 2 ]; then
  echo 'Usage: build.sh /absolute/wendyos /absolute/private/world-site-localize' >&2
  exit 2
fi
source_dir="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$source_dir/../.." && pwd)"
wendyos="$(cd "$1" && pwd)"
output="$2"
python3 - "$root" "$output" <<'PY'
from pathlib import Path
import sys
root, out = map(Path, sys.argv[1:])
if not out.is_absolute() or out.resolve().is_relative_to(root.resolve()):
    raise SystemExit('Build output must be an absolute path outside the repository')
out.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
PY
app="$wendyos/go/macos"
lib="$app/Build/VisualRecognition/macosx/install"
if [ ! -f "$lib/lib/libopencv_calib3d.a" ]; then
  echo 'WendyOS OpenCV build is missing. Build its VisualRecognition dependency first.' >&2
  exit 2
fi
umask 077
xcrun clang++ -std=c++17 -O2 -mmacosx-version-min=26.0 \
  -I"$app/Sources/Shared" -I"$lib/include/opencv4" \
  "$source_dir/main.cpp" "$app/Sources/App/SiteLocalization.cpp" \
  -L"$lib/lib" -lopencv_calib3d -lopencv_features2d -lopencv_flann -lopencv_imgproc -lopencv_core \
  "$lib/lib/opencv4/3rdparty/libtegra_hal.a" -lz -o "$output"
python3 - "$output" "$source_dir/main.cpp" "$app/Sources/App/SiteLocalization.cpp" "$app/Sources/Shared/SiteLocalization.h" <<'PY'
import hashlib,json,sys
from pathlib import Path
out=Path(sys.argv[1])
record={'schema':'wendy.site-localization.build.v1','binary_sha256':hashlib.sha256(out.read_bytes()).hexdigest(),
        'sources':{p:hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in sys.argv[2:]},
        'minimum_macos':'26.0','solver':'existing WendyOS SiteLocalization.cpp'}
Path(str(out)+'.build.json').write_text(json.dumps(record,indent=2)+'\n')
PY
echo "$output"
