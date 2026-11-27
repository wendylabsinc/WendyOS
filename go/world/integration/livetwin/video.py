"""Encode audited original native-window captures, without replaying state.

Run with a Python environment containing Pillow and local ffmpeg/ffprobe:
  python -m integration.livetwin.video --run-dir RUN --out FRESH

The movie preserves capture-start intervals through the final capture finish.
It holds each recorded PNG until the next capture, without interpolation.
Captions describe the observed switches and the app's own freshness receipt.
This is a recorded automated native walkthrough of MuJoCo 4 simulation.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import time


MAX_FRAMES = 256
MAX_DURATION_NS = 45_000_000_000


def require(value, message):
    if not value:
        raise ValueError(message)


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def read(path, maximum):
    require(not path.is_symlink() and path.is_file(), 'Expected a regular input file')
    require(0 < path.stat().st_size <= maximum, 'Input exceeds its size bound')
    raw = path.read_bytes()
    require(0 < len(raw) <= maximum, 'Input changed beyond its size bound')
    return raw


def relative(root, name):
    require(isinstance(name, str) and not Path(name).is_absolute(), 'Expected a relative capture path')
    parts = name.split('/')
    require(0 < len(parts) <= 8 and all(p not in ('', '.', '..') for p in parts), 'Invalid capture path')
    path = root
    for part in parts:
        path = path / part
        require(not path.is_symlink(), 'Capture path contains a symlink')
    return path


def load_frames(run_dir):
    raw = read(run_dir / 'live/native-window.json', 4 << 20)
    window = json.loads(raw)
    frames = window['frames']
    require(isinstance(frames, list) and 2 <= len(frames) <= MAX_FRAMES, 'Expected bounded captured frames')
    previous_end = -1
    run_id = None
    selected = []
    for item in frames:
        sidecar_path = relative(run_dir, item['receipt'])
        sidecar_raw = read(sidecar_path, 32 << 10)
        sidecar = json.loads(sidecar_raw)
        require(all(item.get(key) == value for key, value in sidecar.items()), 'Window index differs from app receipt')
        require(sidecar['schema'] == 'wendy.macos-demo.app-capture.v1'
                and sidecar['mode'] == 'live-native-replica'
                and sidecar['display_clock'] == 'mach_absolute_time_ns', 'Unexpected capture mode or clock')
        if run_id is None:
            run_id = sidecar['run_id']
        require(sidecar['run_id'] == run_id, 'Mixed capture runs')
        start, finish = (sidecar[key] for key in
                         ('capture_started_monotonic_ns', 'capture_finished_monotonic_ns'))
        require(type(start) is int and type(finish) is int and 0 <= start < finish
                and start >= previous_end, 'Capture intervals are not chronological')
        previous_end = finish
        image_path = relative(run_dir, item['file'])
        require(image_path.suffix == '.png' and item['receipt'] == item['file'] + '.json', 'Unexpected capture asset')
        image_raw = read(image_path, 20 << 20)
        require(digest(image_raw) == sidecar['png_sha256'] and len(image_raw) == sidecar['png_bytes'], 'PNG does not match app receipt')
        selected.append(dict(file=item['file'], sidecar=item['receipt'], receipt=sidecar,
                             sidecar_sha256=digest(sidecar_raw), start_ns=start, finish_ns=finish))
    duration = selected[-1]['finish_ns'] - selected[0]['start_ns']
    require(0 < duration <= MAX_DURATION_NS, 'Capture duration exceeds 45 seconds')
    return run_id, selected, duration, digest(raw)


def caption(receipt, restored):
    sources = {row['id']: row['enabled'] for row in receipt.get('sensor_sources', [])}
    enabled = sources.get('camera-b')
    if enabled is False:
        others = all(value for key, value in sources.items() if key != 'camera-b')
        return ('Camera B paused',
                'The other sensors keep running.' if others else 'The switches show which sensors are active.')
    if receipt.get('current_native_hazard_captured') is True:
        return ('Fresh hazard estimate restored' if restored else 'Fusion across two gateways',
                'Camera, LiDAR, gas and audio form one shared estimate.')
    if receipt.get('hazard_visible') is True:
        return ('Last observation expired', 'Waiting for the next sensor update.')
    return ('Camera B restored; waiting for an update' if restored else 'Waiting for the next sensor update',
            'The twin shows the available observations.')


def compose(image_path, destination, receipt, elapsed, restored, fonts, *, captioner=caption, subtitle='Live sensor fusion'):
    # Composition only. The application image is fitted intact, never repainted.
    from PIL import Image, ImageDraw
    with Image.open(image_path) as original:
        require(original.format == 'PNG' and 1 <= original.width <= 8192
                and 1 <= original.height <= 8192, 'Invalid captured PNG dimensions')
        image = original.convert('RGB')
    canvas = Image.new('RGB', (1920, 1080), '#eef1ef')
    draw = ImageDraw.Draw(canvas)
    draw.text((48, 24), 'WENDY WORLD', font=fonts['brand'], fill='#145f58')
    draw.text((285, 26), subtitle, font=fonts['subtitle'], fill='#334e4b')
    draw.text((1872, 28), 'RECORDED SIMULATION  /  MUJOCO 4', font=fonts['small'], fill='#4e6965', anchor='ra')
    available = (1824, 874)
    image.thumbnail(available, Image.Resampling.LANCZOS)
    x, y = (1920 - image.width) // 2, 78 + (874 - image.height) // 2
    draw.rounded_rectangle((x-2, y-2, x+image.width+2, y+image.height+2), radius=6, fill='#c7d3ce')
    canvas.paste(image, (x, y))
    title, detail = captioner(receipt, restored)
    draw.text((48, 975), title, font=fonts['caption'], fill='#173d38')
    draw.text((48, 1018), detail, font=fonts['body'], fill='#526762')
    draw.text((1872, 979), f'{elapsed:04.1f}s', font=fonts['caption'], fill='#176e63', anchor='ra')
    draw.text((1872, 1045), 'Automated walkthrough · native app controls',
              font=fonts['small'], fill='#6b7c76', anchor='ra')
    canvas.save(destination, format='PNG')
    return title, detail


def encode(run_dir, out, *, proof_module=None, captioner=caption, subtitle='Live sensor fusion'):
    if proof_module is None:
        from integration.livetwin import proof as proof_module
    proof = proof_module
    audit = proof.audit(run_dir)
    require(audit.get('status') == 'complete' and audit.get('passed') is True
            and audit.get('source_run_passed') is True and audit.get('live_nonreplay') is True,
            'A complete passing live-run proof audit is required before encoding')
    run_id, frames, duration_ns, window_hash = load_frames(run_dir)
    require(out.is_absolute() and not out.exists() and not out.is_symlink(), 'Choose a fresh absolute output directory')
    require(all(not parent.is_symlink() for parent in out.parents), 'Output ancestors must not be symlinks')
    from PIL import ImageFont
    fonts_dir = Path('/System/Library/Fonts/Supplemental')
    regular, bold = fonts_dir / 'Arial.ttf', fonts_dir / 'Arial Bold.ttf'
    fonts = {name: ImageFont.truetype(str(bold if strong else regular), size) for name, size, strong in
             [('brand', 26, True), ('subtitle', 24, False), ('small', 17, False),
              ('caption', 29, True), ('body', 22, False)]}
    ffmpeg, ffprobe = shutil.which('ffmpeg'), shutil.which('ffprobe')
    require(ffmpeg and ffprobe, 'Local ffmpeg and ffprobe are required')
    os.umask(0o077)
    out.mkdir(mode=0o700, parents=True)
    composed = out / 'frames'; composed.mkdir(mode=0o700)
    (out / 'audit.json').write_text(json.dumps(audit, indent=2, sort_keys=True)+'\n')
    origin = frames[0]['start_ns']
    saw_off = False
    inventory, concat = [], ['ffconcat version 1.0']
    for index, frame in enumerate(frames):
        receipt = frame['receipt']
        enabled = next((row['enabled'] for row in receipt.get('sensor_sources', []) if row['id'] == 'camera-b'), None)
        saw_off = saw_off or enabled is False
        name = f'frames/{index:04d}.png'
        title, detail = compose(relative(run_dir, frame['file']), out / name, receipt,
                                (frame['start_ns']-origin)/1e9, saw_off and enabled is True, fonts,
                                captioner=captioner, subtitle=subtitle)
        until = frames[index+1]['start_ns'] if index+1 < len(frames) else frame['finish_ns']
        span = until-frame['start_ns']
        concat.extend([f"file '{name}'", 'option framerate 1000000', f'duration {span/1e9:.9f}'])
        inventory.append(dict(source=frame['file'], sidecar=frame['sidecar'],
            source_sha256=receipt['png_sha256'], source_bytes=receipt['png_bytes'],
            sidecar_sha256=frame['sidecar_sha256'], capture_started_monotonic_ns=frame['start_ns'],
            capture_finished_monotonic_ns=frame['finish_ns'], at_ns=frame['start_ns']-origin,
            duration_ns=span, composition=name, composition_sha256=digest((out/name).read_bytes()),
            caption=title, detail=detail, state_sequence=receipt['state_sequence'],
            current_native_hazard_captured=receipt.get('current_native_hazard_captured') is True,
            inspector_panel=receipt.get('inspector_panel'),
            current_visible_tracks=sum(track.get('visible') is True and track.get('fresh_at_start') is True
                                       and track.get('fresh_at_end') is True for track in receipt.get('native_tracks', []))))
    # A final timestamp lets the muxer retain the measured last capture duration.
    concat.extend([f"file 'frames/{len(frames)-1:04d}.png'", 'option framerate 1000000'])
    (out/'frames.ffconcat').write_text('\n'.join(concat)+'\n')
    commands = []
    def execute(argv, label, timeout):
        started = time.monotonic()
        with (out/(label+'.stdout')).open('xb') as stdout, (out/(label+'.stderr')).open('xb') as stderr:
            result = subprocess.run(argv, cwd=out, stdout=stdout, stderr=stderr, timeout=timeout)
        commands.append(dict(argv=argv, exit_code=result.returncode, elapsed_s=time.monotonic()-started))
        (out/'commands.json').write_text(json.dumps(commands, indent=2)+'\n')
        require(result.returncode == 0, label+' failed; inspect retained logs')
    video = out/'wendy-world-live-native-fusion.mp4'
    execute([ffmpeg, '-hide_banner', '-loglevel', 'warning', '-safe', '0', '-f', 'concat',
             '-i', 'frames.ffconcat', '-fps_mode', 'vfr', '-enc_time_base', '1:1000000',
             '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-pix_fmt', 'yuv420p',
             '-video_track_timescale', '1000000', '-movflags', '+faststart', '-an',
             '-t', f'{duration_ns/1e9:.9f}', str(video)], 'encode', 600)
    execute([ffprobe, '-v', 'error', '-show_streams', '-show_format', '-of', 'json', str(video)], 'probe', 30)
    info = json.loads((out/'probe.stdout').read_bytes())
    stream = next(row for row in info['streams'] if row['codec_type'] == 'video')
    require(stream['width'] == 1920 and stream['height'] == 1080
            and abs(float(info['format']['duration'])-duration_ns/1e9) <= .002,
            'Encoded dimensions or actual duration differ')
    before = audit['selected']['before_camera_off']
    poster_index = next(index for index, row in enumerate(inventory) if row['source'] == before['frame'])
    # Prefer camera evidence in the poster while retaining that same audited
    # original hazard. Every candidate's complete capture is current and its
    # original PNG remains unchanged in the recorded sequence.
    candidates = [index for index, frame in enumerate(frames)
                  if frame['receipt'].get('current_native_hazard_captured') is True
                  and frame['receipt'].get('original_envelope_sha256') == before['envelope_sha256']
                  and frame['finish_ns'] < audit['selected']['camera_off_empty']['captured_at']]
    if candidates:
        poster_index = max(candidates, key=lambda index: frames[index]['receipt'].get('loaded_thumbnails', 0))
    if audit['selected'].get('together'):
        poster_index = next(index for index, row in enumerate(inventory)
                            if row['source'] == audit['selected']['together']['frame'])
    shutil.copyfile(out/inventory[poster_index]['composition'], out/'poster.png')
    # Recheck retained original inputs after encoding. Publication is the final manifest.
    require(digest(read(run_dir/'live/native-window.json', 4 << 20)) == window_hash, 'Window index changed')
    for row in inventory:
        require(digest(read(relative(run_dir, row['source']), 20 << 20)) == row['source_sha256']
                and digest(read(relative(run_dir, row['sidecar']), 32 << 10)) == row['sidecar_sha256'],
                'Original capture changed during encoding')
    manifest = dict(schema='wendy.live-native-video.v1', status='complete', run_id=run_id,
        source_run=str(run_dir), native_window_sha256=window_hash, duration_ns=duration_ns,
        encoder_sha256=digest(Path(__file__).read_bytes()),
        proof_source_sha256=digest(Path(proof.__file__).read_bytes()),
        caption_source_sha256=digest(Path(__import__(captioner.__module__, fromlist=['__file__']).__file__).read_bytes()),
        audit_sha256=digest((out/'audit.json').read_bytes()),
        timing='Original capture-start intervals through final capture finish; VFR microsecond timebase',
        interpolation=False, state_replay=False, os_input=False, automated_native_walkthrough=True,
        simulation='MuJoCo 4; two native Wendy Agents and ordinary apps', frames=inventory,
        video=dict(file=video.name, sha256=digest(video.read_bytes()), bytes=video.stat().st_size),
        poster=dict(file='poster.png', source_frame=poster_index, sha256=digest((out/'poster.png').read_bytes())),
        fonts={str(path): digest(path.read_bytes()) for path in (regular, bold)}, commands=commands)
    (out/'manifest.json').write_text(json.dumps(manifest, indent=2, sort_keys=True)+'\n')
    return video


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    print(encode(args.run_dir.resolve(strict=True), args.out))


if __name__ == '__main__':
    main()
