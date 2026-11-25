"""Private, byte-preserving import/export of Wendy's WENDYST1/2 site archives.

The native Wendy iPhone app owns scanning and lens calibration. This adapter
accepts its shared site without exporting credentials or rewriting source data.
"""
import base64
import hashlib
import json
import math
from pathlib import Path
import struct
import uuid

MODEL_LIMIT = 128 * 1024 * 1024
MAP_LIMIT = 12 * 1024 * 1024
MANIFEST_LIMIT = 1024 * 1024
ARCHIVE_LIMIT = MODEL_LIMIT + MAP_LIMIT + MANIFEST_LIMIT + 20


def read_bounded(path, limit):
    path = Path(path)
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError('Site input must be a bounded regular file: ' + path.name)
    with path.open('rb') as stream:
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError('Site input exceeds its size limit')
    return data


def validate(manifest, model, visual=None):
    site = json.loads(manifest)
    if site.get('version') != 1:
        raise ValueError('Unsupported site version')
    uuid.UUID(site['id'])
    uuid.UUID(site['modelRevision'])
    scale = site.get('metresPerModelUnit')
    if not isinstance(scale, (float, int)) or isinstance(scale, bool) or not math.isfinite(scale) or not 0 < scale <= 1000:
        raise ValueError('Invalid model scale')
    if not isinstance(site.get('name'), str) or not site['name'].strip() or len(site['name']) > 200:
        raise ValueError('Invalid site name')
    cameras = site.get('cameras')
    if not isinstance(cameras, list) or len(cameras) > 256:
        raise ValueError('Invalid site cameras')
    identities, ids = set(), set()
    for camera in cameras:
        ident = str(uuid.UUID(camera['id']))
        source = (camera.get('gatewayID'), camera.get('sourceID'))
        if any(not isinstance(s, str) or not s or len(s) > 1024 for s in source) or ident in ids or source in identities:
            raise ValueError('Invalid or duplicate site camera identity')
        if not isinstance(camera.get('name'), str) or len(camera['name']) > 200:
            raise ValueError('Invalid camera name')
        if camera.get('position') is not None:
            values = [camera['position'][k] for k in ('x', 'y', 'z')]
            if any(not isinstance(v, (float, int)) or isinstance(v, bool) or not math.isfinite(v) or abs(v) > 1e6 for v in values):
                raise ValueError('Invalid saved camera position')
        ids.add(ident)
        identities.add(source)
    if not 4 <= len(model) <= MODEL_LIMIT or not model.startswith(b'PK\x03\x04'):
        raise ValueError('Invalid USDZ model')
    if visual is not None:
        reference = json.loads(visual)
        if reference.get('version') != 1 or reference.get('modelRevision') != site['modelRevision'] or reference.get('modelSHA256') != hashlib.sha256(model).hexdigest():
            raise ValueError('Visual references do not match this exact model and revision')
        frames = reference.get('frameCount')
        captured = reference.get('capturedAt')
        if not isinstance(frames, int) or isinstance(frames, bool) or not 1 <= frames <= 200 or not isinstance(captured, (int, float)) or not math.isfinite(captured):
            raise ValueError('Invalid visual reference metadata')
        landmarks = base64.b64decode(reference['landmarks'], validate=True)
        if len(landmarks) % 524 or not 120 <= len(landmarks) // 524 <= 12000:
            raise ValueError('Invalid visual reference count')
        for index, (value,) in enumerate(struct.iter_unpack('<f', landmarks)):
            if not math.isfinite(value) or (abs(value) > 1000 if index % 131 < 3 else not 0 <= value <= 512):
                raise ValueError('Invalid visual reference point')
    return site


def read_site(directory):
    directory = Path(directory)
    if directory.is_symlink() or not directory.is_dir():
        raise ValueError('Choose a regular site directory')
    manifest = read_bounded(directory / 'site.json', MANIFEST_LIMIT)
    model = read_bounded(directory / 'model.usdz', MODEL_LIMIT)
    path = directory / 'visual-map.json'
    visual = read_bounded(path, MAP_LIMIT) if path.exists() or path.is_symlink() else None
    return validate(manifest, model, visual), manifest, model, visual


def decode_archive(path):
    data = read_bounded(path, ARCHIVE_LIMIT)
    if len(data) < 16 or data[:8] not in (b'WENDYST1', b'WENDYST2'):
        raise ValueError('Choose a Wendy .wendysite archive')
    manifest_size = struct.unpack('>I', data[8:12])[0]
    if not 0 < manifest_size <= MANIFEST_LIMIT:
        raise ValueError('Invalid site manifest size')
    visual = None
    if data[:8] == b'WENDYST1':
        manifest, model = data[12:12 + manifest_size], data[12 + manifest_size:]
    else:
        if len(data) < 20:
            raise ValueError('Truncated site archive')
        model_size, map_size = struct.unpack('>II', data[12:20])
        if not 0 < model_size <= MODEL_LIMIT or not 0 < map_size <= MAP_LIMIT or len(data) != 20 + manifest_size + model_size + map_size:
            raise ValueError('Invalid site archive lengths')
        start = 20 + manifest_size
        manifest, model, visual = data[20:start], data[start:start + model_size], data[start + model_size:]
    validate(manifest, model, visual)
    return manifest, model, visual


def import_archive(path, destination):
    manifest, model, visual = decode_archive(path)
    destination = Path(destination)
    destination.mkdir(mode=0o700, parents=False, exist_ok=False)
    for name, data in (('site.json', manifest), ('model.usdz', model), ('visual-map.json', visual)):
        if data is not None:
            with (destination / name).open('xb') as stream:
                stream.write(data)
            (destination / name).chmod(0o600)
    return destination


def export_archive(directory, destination):
    _, manifest, model, visual = read_site(directory)
    if visual is None:
        header = b'WENDYST1' + struct.pack('>I', len(manifest))
    else:
        header = b'WENDYST2' + struct.pack('>III', len(manifest), len(model), len(visual))
    with Path(destination).open('xb') as stream:
        stream.write(header)
        stream.write(manifest)
        stream.write(model)
        if visual is not None:
            stream.write(visual)
    Path(destination).chmod(0o600)


def list_sites(root):
    result = []
    if Path(root).is_dir():
        for directory in sorted(Path(root).iterdir()):
            if directory.name.startswith('.') or directory.name == 'RecentlyRemoved' or directory.is_symlink() or not directory.is_dir():
                continue
            try:
                site = json.loads(read_bounded(directory / 'site.json', MANIFEST_LIMIT))
                result.append({'id': site['id'], 'name': site['name'], 'path': str(directory),
                               'visual_references': (directory / 'visual-map.json').is_file()})
            except (OSError, ValueError, KeyError, TypeError):
                continue
    return result
