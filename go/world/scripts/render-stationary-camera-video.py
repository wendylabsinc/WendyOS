#!/usr/bin/env python3
"""Render verified native camera-controller frames into a private review video."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--timeline', type=Path, required=True)
parser.add_argument('--out', type=Path, required=True, help='new private directory outside Git')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
source, out = args.timeline.resolve(), args.out.resolve()
if source.is_relative_to(root) or out.is_relative_to(root) or out.exists():
    parser.error('recordings and new video output must stay outside the repository')
data = json.loads(source.read_text())
if data['schema'] != 'wendy.stationary-camera.recording.v1' or data['provenance'] not in ('simulated', 'measured'):
    parser.error('this recipe requires an explicitly simulated or measured native recording')
if not data.get('presentation'):
    parser.error('recording must describe its capture and interaction method')
frames = data['frames']
if not frames or len(frames) > 1000:
    parser.error('recording frame count is outside the bound')
out.mkdir(parents=True, mode=0o700)
concat = ['ffconcat version 1.0']
for i, frame in enumerate(frames):
    if frame.get('origin') != data['provenance']:
        parser.error('recorded frame provenance differs from the recording')
    path = (source.parent / frame['file']).resolve()
    if path.parent != source.parent or hashlib.sha256(path.read_bytes()).hexdigest() != frame['image_sha256']:
        parser.error('recorded frame path or digest mismatch')
    end = frames[i + 1]['elapsed_s'] if i + 1 < len(frames) else data['duration_s']
    duration = end - frame['elapsed_s']
    # Native view and first Vision initialization can take several seconds. Preserve that recorded
    # time instead of shortening it or inventing intermediate UI states.
    if duration <= 0 or duration > 10:
        parser.error('recorded frame timing is invalid')
    escaped = str(path).replace("'", "'\\''")
    concat += ["file '" + escaped + "'", f'duration {duration:.9f}']
concat.append(concat[-2])
(out / 'frames.ffconcat').write_text('\n'.join(concat) + '\n')
# The native recorder renders the provenance and stage captions with the
# actual app view. Encoding does not synthesize controls or change their state.
video = out / 'stationary-camera-preview.mp4'
subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-f', 'concat', '-safe', '0', '-i', str(out / 'frames.ffconcat'),
                '-vf', 'scale=1920:-2', '-r', '30', '-c:v', 'libx264', '-crf', '19', '-pix_fmt', 'yuv420p', '-movflags', '+faststart',
                '-t', str(data['duration_s']), str(video)], check=True)
(out / 'manifest.json').write_text(json.dumps({
    'schema': 'wendy.stationary-camera.video.v1', 'source_timeline_sha256': hashlib.sha256(source.read_bytes()).hexdigest(),
    'video_sha256': hashlib.sha256(video.read_bytes()).hexdigest(), 'video_bytes': video.stat().st_size,
    'duration_s': data['duration_s'], 'recorded_frames': len(frames), 'provenance': data['provenance'],
    'presentation': data['presentation'], 'actions': data['actions']
}, indent=2) + '\n')
print(video)
