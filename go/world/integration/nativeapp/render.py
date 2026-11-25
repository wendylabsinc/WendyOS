#!/usr/bin/env python3
"""Render private recorded simulator states through the actual native app UI."""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import time

from integration.nativeapp import executable, stop_owned

def metadata(raw):
    return {'bytes': len(raw), 'sha256': hashlib.sha256(raw).hexdigest()}


def bounded(path, limit):
    if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= limit:
        raise ValueError('Expected bounded regular file: ' + str(path))
    raw = path.read_bytes()
    if len(raw) > limit:
        raise ValueError('File exceeds byte limit')
    return raw


def relative(root, name):
    path = Path(name)
    if path.is_absolute() or not path.parts or any(p in ('', '.', '..') for p in name.split('/')):
        raise ValueError('Invalid timeline-relative asset')
    return root / path


def timeline_inputs(path):
    geometry = bounded(path / 'geometry.json', 64 << 20)
    raw = bounded(path / 'frames.jsonl', 512 << 20)
    frames = [json.loads(line) for line in raw.splitlines() if line]
    if not 1 <= len(frames) <= 3600:
        raise ValueError('Invalid timeline length')
    run_id = json.loads(geometry)['run_id']
    previous = (-1, -1, -1)
    assets = {}
    for frame in frames:
        state = frame['state']
        stamp = frame['at_s']
        current = (stamp, state['sequence'], state['snapshot']['time_s'])
        if (not math.isfinite(stamp) or not 0 <= stamp <= 60 or stamp <= previous[0]
                or current[1] < previous[1] or current[2] < previous[2]
                or state['run_id'] != run_id or state['snapshot']['run_id'] != run_id):
            raise ValueError('Timeline scope or ordering differs')
        previous = current
        mapping = frame.get('assets', {})
        for name, source in mapping.items():
            relative(path, name)
            source_path = relative(path, source)
            if source not in assets:
                assets[source] = bounded(source_path, 4 << 20)
        displayed = list((state['snapshot'].get('fusion') or {}).get('observations', []))
        displayed += (state['snapshot'].get('tracking') or {}).get('cameras', [])
        for observation in displayed:
            if observation.get('evidence_path') and observation['evidence_path'] not in mapping:
                raise ValueError('Recorded camera observation lacks its immutable asset')
    if not 0 <= frames[0]['at_s'] < 1:
        raise ValueError('Timeline must begin within the first second')
    return geometry, raw, frames, assets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--timeline', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--duration', type=float, default=40)
    parser.add_argument('--fps', type=int, default=12)
    parser.add_argument('--poster-at', type=float, default=7)
    parser.add_argument('--app', type=Path, help='integrated Wendy.app or WendyDesktop executable')
    args = parser.parse_args()
    if not 0 < args.duration <= 60 or not 1 <= args.fps <= 24:
        parser.error('Use duration up to 60 seconds and 1–24 native frames per second')
    if not args.out.is_absolute() or args.out.exists():
        parser.error('Choose a fresh absolute private output directory')
    geometry, timeline, frames, assets = timeline_inputs(args.timeline)
    binary = Path(executable(args.app))
    binary_metadata = metadata(bounded(binary, 512 << 20))
    os.umask(0o077)
    args.out.mkdir(parents=True, mode=0o700)
    recorded = args.out / 'timeline'; recorded.mkdir()
    manifest = {'schema': 'wendy.native-promo.inputs.v2', 'timeline': {},
                'app': str(binary), 'binary': binary_metadata}
    for name, raw in {'geometry.json': geometry, 'frames.jsonl': timeline, **assets}.items():
        destination = relative(recorded, name)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(raw)
        manifest['timeline'][name] = metadata(raw)
    (args.out / 'inputs.json').write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    commands = []

    def execute(argv, name, timeout):
        began = time.monotonic()
        with (args.out / (name + '.stdout')).open('x') as stdout, (args.out / (name + '.stderr')).open('x') as stderr:
            result = subprocess.run(argv, stdout=stdout, stderr=stderr, timeout=timeout)
        commands.append({'argv': argv, 'exit_code': result.returncode, 'elapsed_s': time.monotonic() - began})
        (args.out / 'commands.json').write_text(json.dumps(commands, indent=2) + '\n')
        if result.returncode:
            raise RuntimeError(name + ' failed; inspect retained output')

    argv = [str(binary), '--world-timeline', str(recorded), '--world-render-output', str(args.out),
            '--world-render-duration', str(args.duration), '--world-render-fps', str(args.fps)]
    began = time.monotonic()
    with (args.out / 'render.stdout').open('x') as stdout, (args.out / 'render.stderr').open('x') as stderr:
        process = subprocess.Popen(argv, stdout=stdout, stderr=stderr, start_new_session=True)
        try:
            deadline = time.monotonic() + 1800
            while not (args.out / 'render.json').exists():
                if (args.out / 'render-error.json').exists():
                    failure = json.loads(bounded(args.out / 'render-error.json', 16384))
                    raise RuntimeError('Integrated renderer did not complete: ' + str(failure.get('error', failure)))
                if process.poll() is not None:
                    raise RuntimeError('Integrated renderer exited before completion; inspect render.stderr')
                if time.monotonic() >= deadline:
                    raise TimeoutError('Integrated renderer deadline expired')
                time.sleep(.1)
            rendered = json.loads(bounded(args.out / 'render.json', 8 << 20))
            if (rendered.get('frame_count') != math.ceil(args.duration * args.fps)
                    or rendered.get('fps') != args.fps or rendered.get('run_id') != json.loads(geometry)['run_id']):
                raise ValueError('Integrated renderer receipt differs from requested capture')
        finally:
            stop_owned(process)
    commands.append({'argv': argv, 'completion': 'validated render.json', 'elapsed_s': time.monotonic()-began})
    ffmpeg = shutil.which('ffmpeg') or 'ffmpeg'
    video = args.out / 'wendy-world-distributed-fusion.mp4'
    execute([ffmpeg, '-hide_banner', '-loglevel', 'warning', '-framerate', str(args.fps),
             '-i', str(args.out / 'frames/%06d.png'), '-vf', 'scale=1920:1080:flags=lanczos,fps=24',
             '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-pix_fmt', 'yuv420p',
             '-movflags', '+faststart', '-an', str(video)], 'encode', 600)
    poster = args.out / 'poster.png'
    poster_index = min(math.ceil(args.duration * args.fps) - 1, max(0, int(args.poster_at * args.fps)))
    shutil.copyfile(args.out / f'frames/{poster_index:06d}.png', poster)
    for name, expected in manifest['timeline'].items():
        if metadata((recorded / name).read_bytes()) != expected:
            raise ValueError('Recorded input changed during rendering')
    if metadata(bounded(binary, 512 << 20)) != binary_metadata:
        raise ValueError('Integrated app changed during rendering')
    execute([shutil.which('ffprobe') or 'ffprobe', '-v', 'error', '-show_streams', '-show_format',
             '-of', 'json', str(video)], 'video-info', 30)
    info = json.loads((args.out / 'video-info.stdout').read_text())
    stream = next(s for s in info['streams'] if s['codec_type'] == 'video')
    if (stream['width'] != 1920 or stream['height'] != 1080 or stream['r_frame_rate'] != '24/1'
            or abs(float(info['format']['duration']) - args.duration) > 1 / args.fps + .05):
        raise ValueError('Encoded video dimensions, frame rate or duration differ')
    receipt = {'schema': 'wendy.native-promo.v1', 'status': 'complete', 'video': metadata(video.read_bytes()),
               'poster': metadata(poster.read_bytes()), 'native_fps': args.fps, 'encoded_fps': 24,
               'duration_s': args.duration, 'timeline_states': len(frames), 'sensor_interpolation': False,
               'scope': 'Offscreen native UI replay of recorded MuJoCo states, not a desktop recording',
               'live_controls': False, 'public_export': False}
    (args.out / 'result.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(video)


if __name__ == '__main__':
    main()
