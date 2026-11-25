#!/bin/bash
set -euo pipefail
if [ "$#" -ne 3 ]; then
  echo 'Usage: test.sh /absolute/wendyos /absolute/bridge /absolute/new-private-output' >&2
  exit 2
fi
source_dir="$(cd "$(dirname "$0")" && pwd)"
app="$(cd "$1/go/macos" && pwd)"
lib="$app/Build/VisualRecognition/macosx/install"
bridge="$2"
out="$3"
python3 - "$source_dir/../.." "$out" <<'PY'
from pathlib import Path
import sys
root,out=map(Path,sys.argv[1:])
if not out.is_absolute() or out.exists() or out.resolve().is_relative_to(root.resolve()):
    raise SystemExit('Test output must be a new absolute path outside the repository')
out.mkdir(parents=True,mode=0o700)
PY
umask 077
xcrun clang++ -std=c++17 -O2 -mmacosx-version-min=26.0 \
  -I"$app/Sources/Shared" -I"$lib/include/opencv4" \
  "$source_dir/fixture.cpp" "$app/Sources/App/SiteLocalization.cpp" \
  -L"$lib/lib" -lopencv_calib3d -lopencv_features2d -lopencv_flann -lopencv_imgproc -lopencv_core \
  "$lib/lib/opencv4/3rdparty/libtegra_hal.a" -lz -o "$out/fixture-generator"
"$out/fixture-generator" "$out/fixture"
python3 - "$bridge" "$out" <<'PY'
from pathlib import Path
import json,math,subprocess,sys
bridge,out=sys.argv[1],Path(sys.argv[2]);fixture=out/'fixture';checks=[]
def run(image='first.pgm',mode='measured',lens='570,570,320,240,0,0,0,0,0',landmarks=None):
    args=[bridge,'--landmarks',str(landmarks or fixture/'map.f32'),'--image',str(fixture/image),'--mode',mode]
    if lens is not None: args+=['--lens',lens]
    p=subprocess.run(args,capture_output=True,text=True,timeout=30)
    return p.returncode,json.loads(p.stdout)
first=None
for image in ['first.pgm','second.pgm']:
    code,r=run(image);assert code==0 and r['success'],r
    assert all(abs(r['world_from_camera'][12+i]-v)<.03 for i,v in enumerate([1,1.5,2])),r
    assert r['fitted_lens']==0 and r['inliers']>=12 and math.isfinite(r['rms'])
    (out/(image+'.result.json')).write_text(json.dumps(r,indent=2)+'\n')
    checks.append('measured pose from '+image)
    first=first or r
for mode,kind in [('joint',1),('estimated',2),('assumed',0)]:
    code,r=run(mode=mode,lens=None if mode!='assumed' else '570,570,320,240,0,0,0,0,0')
    assert code==0 and r['success'] and r['fitted_lens']==kind,r
    assert r['mode']==mode and r['lens_assumed']==(mode in ['estimated','assumed']),r
    assert all(abs(r['world_from_camera'][12+i]-v)<.03 for i,v in enumerate([1,1.5,2])),r
    (out/(mode+'.result.json')).write_text(json.dumps(r,indent=2)+'\n')
    checks.append(mode+' preserves solver lens classification and metric pose')
code,r=run('blank.pgm');assert code==0 and not r['success'];checks.append('blank frame has no pose')
code,r=run(lens='nan,570,320,240,0,0,0,0,0');assert code==2 and not r['success'];checks.append('nonfinite lens rejected')
(fixture/'bad-map.f32').write_bytes(b'\0'*12)
code,r=run(landmarks=fixture/'bad-map.f32');assert code==2;checks.append('short map rejected')
code,r=run(mode='joint');assert code==2;checks.append('unexpected supplied lens rejected')
(fixture/'oversize.pgm').write_bytes(b'P5\n8193 480\n255\n')
code,r=run('oversize.pgm');assert code==2;checks.append('oversize image rejected')
report={'status':'passed','checks':checks,'count':len(checks),'origin':'simulated','scope':'actual existing WendyOS solver via World process bridge'}
(out/'checks.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
PY
