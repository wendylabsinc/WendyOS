#!/usr/bin/env python3
"""Measure private camera intrinsics from checkerboard views; never guess factory values."""
import argparse
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
MAX_IMAGES = 100
MAX_IMAGE_BYTES = 16 * 1024 * 1024
MAX_PIXELS = 32_000_000
MAX_DIMENSION = 8192
MIN_TRAIN = 12
MIN_VALIDATION = 3


def private_output(path):
    path = Path(path).expanduser().resolve()
    if path == ROOT or ROOT in path.parents:
        raise ValueError('Calibration images and artifacts must be outside the repository')
    return path


def write_json(path, value):
    with Path(path).open('x', encoding='utf-8') as f:
        json.dump(value, f, indent=2, allow_nan=False)
        f.write('\n')


def board_spec(columns, rows, square_mm):
    if not 4 <= columns <= 15 or not 4 <= rows <= 15 or not 5 <= square_mm <= 50:
        raise ValueError('Use 4..15 inner corners per axis and 5..50 mm squares')
    return {'inner_columns': columns, 'inner_rows': rows, 'square_mm': square_mm}


def target_pdf(path, columns=9, rows=6, square_mm=20):
    """Dependency-free vector PDF. Exact points preserve the printed target scale."""
    board_spec(columns, rows, square_mm)
    mm = 72 / 25.4
    page_w, page_h = 297 * mm, 210 * mm
    size = square_mm * mm
    bw, bh = (columns + 1) * size, (rows + 1) * size
    if bw > 257 * mm or bh > 155 * mm:
        raise ValueError('Target does not fit A4 landscape with a white border; reduce square size')
    x0, y0 = (page_w - bw) / 2, (page_h - bh) / 2
    content = ['1 g 0 0 %.6f %.6f re f' % (page_w, page_h), '0 g']
    for row in range(rows + 1):
        for col in range(columns + 1):
            if (row + col) % 2 == 0:
                content.append('%.6f %.6f %.6f %.6f re f' % (x0 + col * size, y0 + row * size, size, size))
    def label(x, y, size_pt, text):
        text = text.replace('\\', '\\\\').replace('(', '\\(').replace(')', '\\)')
        content.append(f'BT /F1 {size_pt} Tf {x:.6f} {y:.6f} Td ({text}) Tj ET')
    label(20 * mm, 197 * mm, 14, 'Wendy camera calibration')
    label(20 * mm, 188 * mm, 9, f'{columns} x {rows} INNER corners | {square_mm:g} mm squares | Print A4 landscape at Actual size / 100%')
    label(20 * mm, 13 * mm, 9, 'Measure the line below: exactly 100 mm. Mount this page flat; keep the white border visible.')
    content.append(f'0.7 w {20*mm:.6f} {8*mm:.6f} m {120*mm:.6f} {8*mm:.6f} l S')
    for x in (20, 120):
        content.append(f'{x*mm:.6f} {6*mm:.6f} m {x*mm:.6f} {10*mm:.6f} l S')
    stream = ('\n'.join(content) + '\n').encode('ascii')
    objects = [b'<< /Type /Catalog /Pages 2 0 R >>', b'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
               f'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 {page_w:.6f} {page_h:.6f}] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>'.encode(),
               b'<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
               b'<< /Length ' + str(len(stream)).encode() + b' >>\nstream\n' + stream + b'endstream']
    data = bytearray(b'%PDF-1.4\n%\xe2\xe3\xcf\xd3\n'); offsets = [0]
    for i, obj in enumerate(objects, 1):
        offsets.append(len(data)); data.extend(f'{i} 0 obj\n'.encode() + obj + b'\nendobj\n')
    xref = len(data)
    data.extend(f'xref\n0 {len(objects)+1}\n0000000000 65535 f \n'.encode())
    for offset in offsets[1:]:
        data.extend(f'{offset:010d} 00000 n \n'.encode())
    data.extend(f'trailer\n<< /Size {len(objects)+1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n'.encode())
    path = private_output(path); path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with path.open('xb') as f: f.write(data)
    return path


def libraries():
    try:
        import cv2
        import numpy as np
    except ImportError as exc:
        raise ValueError('Fitting needs OpenCV and NumPy. Use the isolated environment in docs/camera-calibration.md') from exc
    return cv2, np


def image_dimensions(data):
    """Bound the decoder allocation before calling OpenCV."""
    if data.startswith(b'\x89PNG\r\n\x1a\n') and len(data) >= 24:
        w, h = struct.unpack('>II', data[16:24])
    elif data.startswith(b'\xff\xd8'):
        pos, w, h = 2, 0, 0
        while pos < len(data):
            if data[pos] != 0xff: raise ValueError('Malformed JPEG header')
            while pos < len(data) and data[pos] == 0xff: pos += 1
            if pos >= len(data): break
            marker = data[pos]; pos += 1
            if marker in (0xd8, 0xd9, 0x01) or 0xd0 <= marker <= 0xd7: continue
            if marker == 0xda: break
            if pos + 2 > len(data): break
            length = int.from_bytes(data[pos:pos+2], 'big')
            if length < 2 or pos + length > len(data): raise ValueError('Truncated JPEG segment')
            if marker in (0xc0, 0xc1, 0xc2):
                if length < 8: raise ValueError('Invalid JPEG dimensions')
                h, w = struct.unpack('>HH', data[pos+3:pos+7]); break
            pos += length
        if not w: raise ValueError('No supported JPEG dimensions found')
    else:
        raise ValueError('Use original JPEG or PNG images')
    if w < 32 or h < 32 or max(w, h) > MAX_DIMENSION or w * h > MAX_PIXELS:
        raise ValueError('Image dimensions exceed calibration bounds')
    return w, h


def read_image(path):
    path = Path(path)
    if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= MAX_IMAGE_BYTES:
        raise ValueError('Image must be a bounded regular file, not a symlink')
    data = path.read_bytes(); size = image_dimensions(data)
    cv2, np = libraries()
    image = cv2.imdecode(np.frombuffer(data, np.uint8), cv2.IMREAD_GRAYSCALE)
    if image is None or image.shape[::-1] != size: raise ValueError('Image could not be decoded at its original size')
    return data, image


def object_points(columns, rows, square_mm):
    _, np = libraries()
    points = np.zeros((columns * rows, 3), np.float32)
    points[:, :2] = np.mgrid[0:columns, 0:rows].T.reshape(-1, 2) * (square_mm / 1000)
    return points


def detect_views(folder, columns, rows):
    cv2, np = libraries()
    folder = Path(folder).expanduser().resolve()
    paths = sorted(p for p in folder.iterdir() if p.suffix.lower() in ('.jpg', '.jpeg', '.png'))
    if not MIN_TRAIN + MIN_VALIDATION <= len(paths) <= MAX_IMAGES:
        raise ValueError(f'Provide {MIN_TRAIN + MIN_VALIDATION}..{MAX_IMAGES} images')
    views, rejected, hashes, size = [], [], set(), None
    for path in paths:
        data, gray = read_image(path); digest = hashlib.sha256(data).hexdigest()
        if digest in hashes:
            rejected.append({'file': path.name, 'reason': 'duplicate_bytes'}); continue
        hashes.add(digest)
        dimensions = gray.shape[::-1]
        if size is not None and dimensions != size: raise ValueError('All views must have exactly the same original dimensions')
        size = dimensions
        ok, corners = cv2.findChessboardCornersSB(gray, (columns, rows), cv2.CALIB_CB_NORMALIZE_IMAGE | cv2.CALIB_CB_EXHAUSTIVE | cv2.CALIB_CB_ACCURACY)
        if not ok:
            rejected.append({'file': path.name, 'reason': 'checkerboard_not_found'}); continue
        corners = corners.reshape(-1, 2).astype(np.float32)
        # Moving less than 2% of the image diagonal gives almost the same view.
        if any(np.sqrt(np.mean(np.sum((corners-v['points'])**2, axis=1))) / math.hypot(*size) < .02 for v in views):
            rejected.append({'file': path.name, 'reason': 'nearly_repeated_view'}); continue
        views.append({'file': path.name, 'sha256': digest, 'points': corners})
    return views, size, rejected


def validate_diversity(views, size, columns, rows):
    cv2, np = libraries()
    if len(views) < MIN_TRAIN + MIN_VALIDATION: raise ValueError('Need at least 15 distinct detected checkerboard views')
    centers = np.array([v['points'].mean(axis=0) for v in views]) / np.array(size)
    if np.ptp(centers[:, 0]) < .15 or np.ptp(centers[:, 1]) < .15:
        raise ValueError('Move the target across both image axes, including the corners')
    areas = []
    for view in views:
        p = view['points']; quad = p[[0, columns - 1, columns * rows - 1, columns * (rows - 1)]]
        area = abs(cv2.contourArea(quad)) / (size[0] * size[1]); areas.append(area)
        if area < .015: raise ValueError('A checkerboard view is too small; bring the target closer')
    if max(areas) / min(areas) < 1.4: raise ValueError('Capture the target at different distances, not just different positions')
    return {'center_span_fraction': np.ptp(centers, axis=0).tolist(), 'min_board_area_fraction': min(areas), 'max_board_area_fraction': max(areas)}


def rms_projected(obj, points, rvec, tvec, matrix, distortion):
    cv2, np = libraries()
    projected, _ = cv2.projectPoints(obj, rvec, tvec, matrix, distortion)
    return float(np.sqrt(np.mean(np.sum((projected.reshape(-1, 2) - points.reshape(-1, 2))**2, axis=1))))


def fit_views(views, size, columns=9, rows=6, square_mm=20):
    cv2, np = libraries()
    diversity = validate_diversity(views, size, columns, rows)
    # Entire views, never points from a fitted view, are held out from K/distortion fitting.
    validation = [v for i, v in enumerate(views) if i % 5 == 4]
    training = [v for i, v in enumerate(views) if i % 5 != 4]
    if len(training) < MIN_TRAIN or len(validation) < MIN_VALIDATION:
        raise ValueError('Need at least 12 fitting views and 3 held-out views')
    obj = object_points(columns, rows, square_mm)
    rms, matrix, distortion, rotations, translations = cv2.calibrateCamera(
        [obj.copy() for _ in training], [v['points'].reshape(-1, 1, 2) for v in training], size, None, None)
    distortion = distortion.reshape(-1)
    if matrix.shape != (3, 3) or len(distortion) != 5 or not np.isfinite(matrix).all() or not np.isfinite(distortion).all():
        raise ValueError('Calibration produced invalid camera parameters')
    fx, fy, cx, cy = float(matrix[0, 0]), float(matrix[1, 1]), float(matrix[0, 2]), float(matrix[1, 2])
    if not (.1*size[0] <= fx <= 5*size[0] and .1*size[1] <= fy <= 5*size[1] and 0 <= cx <= size[0] and 0 <= cy <= size[1]) or np.abs(distortion).max() > 5:
        raise ValueError('Calibration parameters are implausible; improve target coverage and tilt')
    normals = [cv2.Rodrigues(r)[0][:, 2] for r in rotations]
    max_tilt = max(math.degrees(math.acos(float(np.clip(np.dot(a, b), -1, 1)))) for a in normals for b in normals)
    if max_tilt < 15: raise ValueError('Views do not vary target tilt enough; include visibly angled targets')
    errors = []
    for view in validation:
        ok, rvec, tvec = cv2.solvePnP(obj, view['points'], matrix, distortion, flags=cv2.SOLVEPNP_ITERATIVE)
        if not ok or float(tvec[2, 0]) <= 0: raise ValueError('A held-out target pose failed verification')
        error = rms_projected(obj, view['points'], rvec, tvec, matrix, distortion)
        errors.append({'file': view['file'], 'rms_pixels': error})
    validation_rms = math.sqrt(sum(v['rms_pixels']**2 for v in errors) / len(errors))
    if not math.isfinite(rms) or not math.isfinite(validation_rms) or rms > 2 or validation_rms > 2 or max(v['rms_pixels'] for v in errors) > 3:
        raise ValueError('Calibration failed held-out accuracy checks: require fitting/validation RMS <=2 px and every held-out view <=3 px')
    return {'intrinsics': {'fx': fx, 'fy': fy, 'cx': cx, 'cy': cy, 'distortion': distortion.tolist()},
            'training_views': len(training), 'validation_views': len(validation), 'training_rms_pixels': float(rms),
            'validation_rms_pixels': validation_rms, 'held_out_errors': errors, 'diversity': {**diversity, 'normal_span_degrees': max_tilt},
            'training_files': [v['file'] for v in training], 'validation_files': [v['file'] for v in validation]}


def camera_fields(args, size):
    values = {k: getattr(args, k) for k in ('board', 'sensor', 'lens', 'capture_mode', 'hardware_id')}
    if any(not isinstance(v, str) or not v.strip() or len(v) > 128 or any(ord(c)<32 for c in v) for v in values.values()):
        raise ValueError('Supply explicit board, sensor, lens, capture mode and hardware identity, each at most 128 characters')
    return {**values, 'width': size[0], 'height': size[1]}


def calibration_profile(args, size, fit, dataset_sha256):
    if any(not isinstance(v, str) or not v.strip() or len(v) > 128 or any(ord(c) < 32 for c in v) for v in (args.id, args.name)):
        raise ValueError('Supply a profile id and name of at most 128 characters')
    camera = camera_fields(args, size)
    if args.scope == 'model': camera.pop('hardware_id')
    return {'schema': 'wendy.camera-intrinsics.v1', 'id': args.id, 'name': args.name, 'scope': args.scope, 'origin': args.origin,
            'camera': camera, 'intrinsics': fit['intrinsics'],
            'evidence': {'method': 'opencv-checkerboard', 'reference_device_ids': [args.hardware_id],
                         **{k: fit[k] for k in ('training_views', 'validation_views', 'training_rms_pixels', 'validation_rms_pixels')},
                         'dataset_sha256': dataset_sha256, 'calibrated_at': dt.datetime.now(dt.timezone.utc).isoformat().replace('+00:00', 'Z')}}


def validate_manifest(folder, views, origin, hardware_id):
    path = folder / 'capture.json'
    if not path.exists(): return {'source': 'operator_supplied_images', 'origin': origin, 'hardware_identity': 'operator_declared'}
    if path.stat().st_size > 2 * 1024 * 1024: raise ValueError('Capture manifest exceeds size limit')
    manifest = json.loads(path.read_text())
    if manifest.get('schema') != 'wendy.camera-calibration.capture.v1' or manifest.get('origin') != origin or manifest.get('hardware_id') != hardware_id:
        raise ValueError('Capture origin or hardware identity does not match the requested calibration')
    indexed = {v['file']: v for v in manifest['frames']}
    for view in views:
        if view['file'] not in indexed or indexed[view['file']]['sha256'] != view['sha256']:
            raise ValueError('Captured image digest differs from its original receipt')
    return {'source': 'wendy_receipts', 'origin': origin, 'hardware_identity': 'operator_declared', 'capture_sha256': hashlib.sha256(path.read_bytes()).hexdigest()}


def fit_command(args):
    folder = Path(args.images).expanduser().resolve()
    board_spec(args.columns, args.rows, args.square_mm)
    views, size, rejected = detect_views(folder, args.columns, args.rows)
    if size is None: raise ValueError('No usable image dimensions')
    provenance = validate_manifest(folder, views, args.origin, args.hardware_id)
    result = fit_views(views, size, args.columns, args.rows, args.square_mm)
    dataset = {'schema': 'wendy.camera-calibration.dataset.v1', 'origin': args.origin, 'board': board_spec(args.columns, args.rows, args.square_mm),
               'images': [{'file': v['file'], 'sha256': v['sha256']} for v in views], 'provenance': provenance}
    encoded = (json.dumps(dataset, sort_keys=True, separators=(',', ':'), allow_nan=False) + '\n').encode()
    profile = calibration_profile(args, size, result, hashlib.sha256(encoded).hexdigest())
    out = private_output(args.out); out.mkdir(parents=True, exist_ok=False, mode=0o700)
    (out / 'dataset.json').write_bytes(encoded)
    cv2, np = libraries()
    write_json(out / 'report.json', {'schema': 'wendy.camera-calibration.report.v1', 'origin': args.origin, 'opencv_version': cv2.__version__, 'numpy_version': np.__version__, 'result': result, 'rejected_views': rejected,
                                   'scope_note': 'A model profile is measured on the listed reference unit only; transfer to other units is not independently verified.' if args.scope == 'model' else 'Measured on the declared device; identity is operator-provided.'})
    write_json(out / 'profile.json', profile); write_json(out / 'binding.json', camera_fields(args, size))
    print(f'Calibration passed. Profile: {out / "profile.json"}')
    print(f'Fitting RMS {result["training_rms_pixels"]:.3f} px; held-out RMS {result["validation_rms_pixels"]:.3f} px.')
    if args.origin == 'simulated': print('SIMULATED calibration: cannot be used for measured camera frames.')
    if args.scope == 'model': print('Model profile: validate transfer on additional units before claiming batch accuracy.')


def fixed_lens_validation(views, size, intrinsics, columns=9, rows=6, square_mm=20):
    """Keep K/distortion fixed. Only per-view target poses are solved."""
    cv2, np = libraries()
    diversity = validate_diversity(views, size, columns, rows)
    values = [intrinsics[k] for k in ('fx', 'fy', 'cx', 'cy')]
    distortion = np.asarray(intrinsics['distortion'], dtype=np.float64)
    if len(distortion) != 5 or not all(isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v) for v in values) or not np.isfinite(distortion).all() or np.abs(distortion).max() > 100:
        raise ValueError('Profile has invalid intrinsics')
    fx, fy, cx, cy = values
    if not (.1*size[0] <= fx <= 10*size[0] and .1*size[1] <= fy <= 10*size[1] and 0 <= cx <= size[0] and 0 <= cy <= size[1]):
        raise ValueError('Profile has invalid intrinsics')
    matrix = np.array([[fx, 0, cx], [0, fy, cy], [0, 0, 1]], dtype=np.float64)
    obj = object_points(columns, rows, square_mm)
    errors, normals = [], []
    for view in views:
        ok, rvec, tvec = cv2.solvePnP(obj, view['points'], matrix, distortion, flags=cv2.SOLVEPNP_ITERATIVE)
        if not ok or not np.isfinite(tvec).all() or float(tvec[2, 0]) <= 0:
            raise ValueError('A validation target pose failed')
        error = rms_projected(obj, view['points'], rvec, tvec, matrix, distortion)
        if not math.isfinite(error): raise ValueError('Validation returned a nonfinite error')
        errors.append({'file': view['file'], 'rms_pixels': error})
        normals.append(cv2.Rodrigues(rvec)[0][:, 2])
    max_tilt = max(math.degrees(math.acos(float(np.clip(np.dot(a, b), -1, 1)))) for a in normals for b in normals)
    if max_tilt < 15: raise ValueError('Validation views do not vary target tilt enough')
    rms = math.sqrt(sum(v['rms_pixels']**2 for v in errors) / len(errors))
    return {'passed': rms <= 2 and max(v['rms_pixels'] for v in errors) <= 3,
            'views': len(views), 'rms_pixels': rms, 'per_view_errors': errors,
            'diversity': {**diversity, 'normal_span_degrees': max_tilt}, 'lens_refitted': False}


def validate_command(args):
    path = Path(args.profile).expanduser()
    if not path.is_file() or path.is_symlink() or path.stat().st_size > 65536:
        raise ValueError('Profile must be a bounded regular JSON file')
    encoded = path.read_bytes(); profile = json.loads(encoded)
    if profile.get('schema') != 'wendy.camera-intrinsics.v1' or profile.get('origin') != args.origin or profile.get('scope') not in ('model', 'device'):
        raise ValueError('Profile schema, scope or origin does not match the requested validation')
    board_spec(args.columns, args.rows, args.square_mm)
    folder = Path(args.images).expanduser().resolve()
    views, size, rejected = detect_views(folder, args.columns, args.rows)
    if size is None: raise ValueError('No usable image dimensions')
    binding = camera_fields(args, size)
    for key in ('board', 'sensor', 'lens', 'capture_mode', 'width', 'height'):
        if profile.get('camera', {}).get(key) != binding[key]:
            raise ValueError(f'Profile camera {key} does not match this validation capture')
    provenance = validate_manifest(folder, views, args.origin, args.hardware_id)
    result = fixed_lens_validation(views, size, profile['intrinsics'], args.columns, args.rows, args.square_mm)
    out = private_output(args.out); out.mkdir(parents=True, exist_ok=False, mode=0o700)
    cv2, np = libraries()
    report = {'schema': 'wendy.camera-calibration.transfer-check.v1', 'origin': args.origin,
              'opencv_version': cv2.__version__, 'numpy_version': np.__version__,
              'profile_id': profile.get('id'), 'profile_sha256': hashlib.sha256(encoded).hexdigest(),
              'camera': binding, 'provenance': provenance, 'result': result, 'rejected_views': rejected,
              'image_digests': [{'file': v['file'], 'sha256': v['sha256']} for v in views],
              'scope_note': 'This checks only the declared unit and views. It does not alter the profile scope or certify a batch.'}
    write_json(out / 'validation.json', report)
    print(f'Fixed-lens validation: {"passed" if result["passed"] else "FAILED"}; RMS {result["rms_pixels"]:.3f} px. Report: {out / "validation.json"}')
    if not result['passed']: raise ValueError('Shared lens profile failed validation on this unit; calibrate an individual override')


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError('Viewer endpoint redirected; credentials were not forwarded')


def viewer_reader(profile_path):
    p = Path(profile_path).expanduser()
    if not p.is_file() or p.stat().st_size > 8192: raise ValueError('Invalid viewer profile')
    profile = json.loads(p.read_text()); url = profile.get('url', '')
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != 'http' or parsed.hostname not in ('127.0.0.1', 'localhost', '::1') or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ('', '/'):
        raise ValueError('Only the local viewer HTTP endpoint is supported')
    token = profile.get('token')
    if not isinstance(token, str) or not token or len(token) > 1024 or '\r' in token or '\n' in token: raise ValueError('Invalid viewer token')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    def read(path, limit):
        request = urllib.request.Request(url.rstrip('/') + path, headers={'Authorization': 'Bearer ' + token})
        with opener.open(request, timeout=8) as response:
            data = response.read(limit + 1)
            if len(data) > limit: raise ValueError('Viewer response exceeds size limit')
            return data
    return read


def capture_command(args):
    if not 15 <= args.count <= MAX_IMAGES or not .5 <= args.interval <= 30 or not 10 <= args.duration <= 1800:
        raise ValueError('Capture requires 15..100 images, 0.5..30 second spacing, and a 10..1800 second duration')
    if not args.hardware_id or len(args.hardware_id) > 128: raise ValueError('Supply the operator-confirmed hardware identity')
    read = viewer_reader(args.viewer_profile)
    cv2, np = libraries()
    board_spec(args.columns, args.rows, args.square_mm)
    out = private_output(args.out); out.mkdir(parents=True, exist_ok=False, mode=0o700)
    manifest = {'schema': 'wendy.camera-calibration.capture.v1', 'origin': None, 'hardware_id': args.hardware_id, 'identity_source': 'operator_declared', 'camera_id': args.camera, 'frames': []}
    deadline = time.monotonic() + args.duration
    seen, size, session, next_save = set(), None, None, 0
    accepted_corners = []
    print('Move the flat checkerboard between captures: vary tilt, distance and position across the image.', flush=True)
    try:
        while len(manifest['frames']) < args.count and time.monotonic() < deadline:
            payload = json.loads(read('/v1/twin', 2 * 1024 * 1024))
            cameras = payload.get('cameras', [])
            selected = [c for c in cameras if c.get('id') == args.camera]
            if not selected:
                # Single-camera older viewers can use the request's default camera-1.
                if len(cameras) == 1 and args.camera == 'camera-1': selected = cameras
                else: raise ValueError('Selected camera was not found in the viewer')
            state = selected[0].get('state', {}); receipt = state.get('latest')
            if state.get('fresh') and receipt and time.monotonic() >= next_save:
                digest = receipt.get('sha256', '')
                if len(digest) != 64 or any(c not in '0123456789abcdef' for c in digest): raise ValueError('Invalid image digest')
                origin = receipt.get('origin'); current_size = (receipt.get('width'), receipt.get('height'))
                if origin not in ('measured', 'simulated') or receipt.get('content_type') != 'image/jpeg' or receipt.get('representation') != 'original_jpeg': raise ValueError('Calibration requires original measured or explicitly simulated JPEG receipts')
                if manifest['origin'] is not None and origin != manifest['origin']: raise ValueError('Camera provenance changed during capture')
                if size is not None and current_size != size: raise ValueError('Camera resolution changed during capture')
                if session is not None and receipt.get('source_session') != session: raise ValueError('Camera reconnected during capture; start a new capture set')
                if digest not in seen:
                    data = read('/v1/evidence/' + digest, MAX_IMAGE_BYTES)
                    if hashlib.sha256(data).hexdigest() != digest or len(data) != receipt.get('size') or image_dimensions(data) != current_size:
                        raise ValueError('Image does not match its receipt')
                    gray = cv2.imdecode(np.frombuffer(data, np.uint8), cv2.IMREAD_GRAYSCALE)
                    if gray is None or gray.shape[::-1] != current_size: raise ValueError('Original JPEG could not be decoded')
                    ok, corners = cv2.findChessboardCornersSB(gray, (args.columns, args.rows), cv2.CALIB_CB_NORMALIZE_IMAGE | cv2.CALIB_CB_EXHAUSTIVE | cv2.CALIB_CB_ACCURACY)
                    seen.add(digest); next_save = time.monotonic() + args.interval
                    if not ok:
                        print('Waiting for the complete checkerboard in view...', flush=True)
                        continue
                    corners = corners.reshape(-1, 2)
                    if any(np.sqrt(np.mean(np.sum((corners - previous)**2, axis=1))) / math.hypot(*current_size) < .02 for previous in accepted_corners):
                        print('Move or tilt the checkerboard for a different view.', flush=True)
                        continue
                    accepted_corners.append(corners)
                    manifest['origin'], size, session = origin, current_size, receipt.get('source_session')
                    name = f'{len(manifest["frames"]):03d}.jpg'; (out / name).write_bytes(data)
                    manifest['frames'].append({'file': name, 'sha256': digest, 'receipt': receipt})
                    seen.add(digest); next_save = time.monotonic() + args.interval
                    print(f'Captured {len(manifest["frames"])}/{args.count}', flush=True)
            time.sleep(.2)
    finally:
        manifest['complete'] = len(manifest['frames']) == args.count
        write_json(out / 'capture.json', manifest)
    if not manifest['complete']: raise ValueError(f'Capture ended with {len(manifest["frames"])} frames; use a new directory to retry')
    print(f'Original images saved privately to {out}. Fitting will reject missing or repeated checkerboard views.')


def parser():
    p = argparse.ArgumentParser(description=__doc__); sub = p.add_subparsers(dest='command', required=True)
    target = sub.add_parser('target', help='create a printable A4 vector PDF without OpenCV')
    target.add_argument('--out', type=Path, required=True)
    fit = sub.add_parser('fit', help='fit intrinsics and verify complete held-out checkerboard views')
    fit.add_argument('--images', type=Path, required=True); fit.add_argument('--out', type=Path, required=True)
    for name in ('id', 'name', 'board', 'sensor', 'lens', 'capture-mode', 'hardware-id'):
        fit.add_argument('--' + name, required=True)
    fit.add_argument('--scope', choices=('device', 'model'), default='device')
    fit.add_argument('--origin', choices=('measured', 'simulated'), required=True, help='must match captured receipts; required operator declaration for an image folder')
    validate = sub.add_parser('validate', help='check an existing profile on another unit without refitting the lens')
    validate.add_argument('--profile', type=Path, required=True)
    validate.add_argument('--images', type=Path, required=True); validate.add_argument('--out', type=Path, required=True)
    for name in ('board', 'sensor', 'lens', 'capture-mode', 'hardware-id'):
        validate.add_argument('--' + name, required=True)
    validate.add_argument('--origin', choices=('measured', 'simulated'), required=True)
    for command in (target, fit, validate):
        command.add_argument('--columns', type=int, default=9, help='inner checkerboard corners horizontally')
        command.add_argument('--rows', type=int, default=6, help='inner checkerboard corners vertically')
        command.add_argument('--square-mm', type=float, default=20)
    capture = sub.add_parser('capture', help='save original frames from an already running local Wendy viewer')
    capture.add_argument('--viewer-profile', type=Path, required=True); capture.add_argument('--out', type=Path, required=True)
    capture.add_argument('--hardware-id', required=True, help='operator-confirmed hardware identity, not inferred from a camera name')
    capture.add_argument('--columns', type=int, default=9); capture.add_argument('--rows', type=int, default=6); capture.add_argument('--square-mm', type=float, default=20)
    capture.add_argument('--camera', default='camera-1'); capture.add_argument('--count', type=int, default=25)
    capture.add_argument('--interval', type=float, default=3); capture.add_argument('--duration', type=float, default=180)
    return p


def main(argv=None):
    os.umask(0o077)
    args = parser().parse_args(argv)
    if args.command == 'target':
        print(target_pdf(args.out, args.columns, args.rows, args.square_mm))
    elif args.command == 'capture': capture_command(args)
    elif args.command == 'validate': validate_command(args)
    else: fit_command(args)
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, urllib.error.URLError) as exc:
        print(f'Calibration stopped: {exc}', file=sys.stderr)
        sys.exit(1)
