#!/usr/bin/env python3
"""Create a private synthetic room around the native solver's texture fixture.

This geometry and imagery are generated test inputs, not a physical room scan.
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import struct
import subprocess
import uuid
import zipfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--solver-fixture', required=True, type=Path)
parser.add_argument('--out', required=True, type=Path)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
out = args.out.expanduser().resolve()
if out.is_relative_to(root) or out.exists():
    parser.error('choose a fresh private directory outside the repository')
out.mkdir(parents=True, mode=0o700)
site = out / 'site'
site.mkdir(mode=0o700)


def cube(name, center, size, color):
    return f'''def Cube "{name}" {{
    double size = 1
    double3 xformOp:translate = {tuple(center)}
    double3 xformOp:scale = {tuple(size)}
    uniform token[] xformOpOrder = ["xformOp:translate", "xformOp:scale"]
    color3f[] primvars:displayColor = [{tuple(color)}]
}}'''


usda = '#usda 1.0\n( metersPerUnit = 1\n upAxis = "Y"\n defaultPrim = "Room" )\ndef Xform "Room" {\n'
for name, center, size, color in [
    ('Floor', (1, -.06, .3), (7, .12, 6), (.86, .88, .86)),
    ('BackWall', (1, 1.5, -2.7), (7, 3, .12), (.72, .81, .8)),
    ('SideWall', (-2.5, 1.5, .3), (.12, 3, 6), (.82, .85, .84)),
    ('Desk', (-.6, .72, -.5), (1.4, .12, .75), (.55, .38, .24)),
    ('DeskLegA', (-1.15, .33, -.5), (.08, .66, .55), (.2, .26, .28)),
    ('DeskLegB', (-.05, .33, -.5), (.08, .66, .55), (.2, .26, .28)),
    ('Cabinet', (3.4, .6, -1.9), (.85, 1.2, .65), (.28, .47, .43)),
]:
    usda += cube(name, center, size, color) + '\n'
usda += '}\n'
with zipfile.ZipFile(site / 'model.usdz', 'x', compression=zipfile.ZIP_STORED) as archive:
    info = zipfile.ZipInfo('room.usda')
    # USDZ requires each member's data to begin at a 64-byte boundary.
    extra_length = (-(30 + len(info.filename))) % 64
    if extra_length < 4:
        extra_length += 64
    info.extra = struct.pack('<HH', 0xCAFE, extra_length - 4) + b'\0' * (extra_length - 4)
    archive.writestr(info, usda.encode())
model = (site / 'model.usdz').read_bytes()
revision = str(uuid.uuid4())
metadata = {'version': 1, 'id': str(uuid.uuid4()), 'name': 'Synthetic studio',
            'modelRevision': revision, 'metresPerModelUnit': 1, 'cameras': []}
(site / 'site.json').write_text(json.dumps(metadata))
(site / 'visual-map.json').write_text(json.dumps({
    'version': 1, 'modelRevision': revision, 'modelSHA256': hashlib.sha256(model).hexdigest(),
    'frameCount': 1, 'capturedAt': 0,
    'landmarks': base64.b64encode((args.solver_fixture / 'map.f32').read_bytes()).decode()}))
subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-i', str(args.solver_fixture / 'first.pgm'),
                '-frames:v', '1', '-q:v', '1', '-pix_fmt', 'yuvj444p', str(out / 'frame.jpg')], check=True)
(out / 'provenance.json').write_text(json.dumps({'origin': 'simulated',
    'scope': 'Generated room geometry and native solver texture/depth references. No physical observations.',
    'model_note': 'Room geometry is for the display check; reference texture/depth is a separate synthetic solver fixture.'}, indent=2))
print(site)
print(out / 'frame.jpg')
