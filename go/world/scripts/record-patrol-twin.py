#!/usr/bin/env python3
"""Record an owned live patrol bridge for private native UI replay.

Captures genuine state and evidence files. Optional interaction mode sends a
pause and resume through the same numbered command/receipt path as the viewer.
No simulator reset, pose assignment, motion command or mission stop is issued.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from integration import nativeapp


def read_json(path, limit=16 << 20):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError(f'Expected bounded regular file: {path}')
    raw = path.read_bytes()
    if len(raw) > limit:
        raise ValueError('File grew past its limit')
    return json.loads(raw)


def write_json(path, value):
    path.write_text(json.dumps(value, allow_nan=False, indent=2) + '\n')


def observed_speed(snapshot):
    robots = snapshot.get('robots') or []
    velocity = robots[0].get('velocity', []) if robots else []
    if len(velocity) < 2 or not all(math.isfinite(v) for v in velocity[:2]):
        return 0.
    return math.hypot(*velocity[:2])


def is_navigating_motion(snapshot):
    patrol = snapshot.get('patrol') or {}
    return (patrol.get('state') == 'navigating'
            and (patrol.get('localization') or {}).get('valid') is True
            and observed_speed(snapshot) >= .1)


def interaction_action(at, duration, sent, pause_acknowledged_at, snapshot):
    if ('patrol_pause' not in sent and 4 <= at <= duration - 10
            and is_navigating_motion(snapshot)):
        return 'patrol_pause'
    if ('patrol_resume' not in sent and pause_acknowledged_at is not None
            and at >= pause_acknowledged_at + 3):
        return 'patrol_resume'
    return None


def capture(session, out, milestone, duration, fps):
    timeline = out / 'timeline'
    timeline.mkdir()
    assets = timeline / 'assets'
    assets.mkdir()
    geometry = read_json(session / 'geometry.json', 64 << 20)
    write_json(timeline / 'geometry.json', geometry)
    run = geometry['run_id']
    started = time.monotonic()
    deadline = started + duration
    next_frame = started
    seen_sequence = -1
    pending = None
    pause_acknowledged_at = None
    paused_speeds, resumed_speeds = [], []
    sent = set()
    positions = []
    simulation_times = []
    pinned_preview = {}
    localized = 0
    frames_count = 0
    report = {'schema': 'wendy.go2-patrol.recording.v1', 'run_id': run,
              'milestone': milestone, 'scope': 'Live simulation states for native UI replay',
              'hardware': False, 'commands': [], 'sensor_interpolation': False}
    with (timeline / 'frames.jsonl').open('x') as stream:
        while time.monotonic() < deadline:
            now = time.monotonic()
            if now < next_frame:
                time.sleep(min(.025, next_frame - now))
                continue
            state = read_json(session / 'state.json')
            if state['run_id'] != run or state.get('mode') != 'go2-patrol':
                raise ValueError('Expected the same live patrol bridge')
            snapshot = state['snapshot']
            patrol = snapshot['patrol']
            if milestone == 'interactions' and frames_count == 0 and patrol['state'] != 'navigating':
                raise ValueError('Interaction recording requires a navigating patrol; no control was issued')
            if milestone == 'interactions' and frames_count == 0 and not is_navigating_motion(snapshot):
                raise ValueError('Interaction recording requires measured motion and valid localization; no control was issued')
            if state['worker_status'] != 'running' or state['sequence'] < seen_sequence:
                raise ValueError('Worker stopped or state sequence regressed')
            if state['sequence'] == seen_sequence:
                if time.monotonic() - last_update > 2:
                    raise ValueError('Bridge stopped producing fresh state')
                time.sleep(.01)
                continue
            seen_sequence = state['sequence']
            last_update = now
            at = now - started
            mapping = {}
            observations = list((snapshot.get('fusion') or {}).get('observations', []))
            observations += (snapshot.get('tracking') or {}).get('cameras', [])
            for observation in observations:
                name = observation.get('evidence_path')
                if not name:
                    continue
                relative = Path(name)
                if relative.is_absolute() or any(p in ('', '.', '..') for p in name.split('/')):
                    raise ValueError('Unsafe evidence path')
                source = session / relative
                if source.is_symlink() or not source.is_file() or source.stat().st_size > 4 << 20:
                    raise ValueError('Camera evidence unavailable')
                raw = source.read_bytes()
                digest = hashlib.sha256(raw).hexdigest()
                expected = observation.get('image_sha256')
                if expected and expected != digest:
                    raise ValueError('Camera changed during capture; recorded state cannot bind it')
                asset = f'assets/{digest}{source.suffix}'
                destination = timeline / asset
                if not destination.exists():
                    destination.write_bytes(raw)
                mapping[name] = asset
            references = set()
            for item in patrol['rooms'] + patrol['recent_findings']:
                references.update(item.get('evidence') or [])
            for digest in references:
                if len(digest) != 64 or any(c not in '0123456789abcdef' for c in digest):
                    raise ValueError('Invalid captured evidence digest')
                for suffix in ('.png', '.wav', '.json', '.bin'):
                    source = session / 'evidence' / (digest + suffix)
                    if not source.exists():
                        continue
                    if source.is_symlink() or not source.is_file() or source.stat().st_size > 4 << 20:
                        raise ValueError('Invalid evidence original')
                    raw = source.read_bytes()
                    if hashlib.sha256(raw).hexdigest() != digest:
                        raise ValueError('Evidence original digest mismatch')
                    asset = 'assets/' + digest + suffix
                    destination = timeline / asset
                    if not destination.exists():
                        destination.write_bytes(raw)
                    mapping['evidence/' + digest + suffix] = asset
                    break
            actions = []
            if milestone == 'interactions' and at >= duration * .55 and 'evidence-open' not in sent:
                images = [name for name in mapping if name.startswith('evidence/') and name.endswith('.png')]
                if images:
                    sent.add('evidence-open')
                    actions.append({'kind': 'open_patrol_evidence', 'target': Path(images[0]).stem})
                    pinned_preview = {images[0]: mapping[images[0]]}
            if milestone == 'interactions' and at >= duration * .8 and 'evidence-open' in sent and 'evidence-close' not in sent:
                sent.add('evidence-close')
                actions.append({'kind': 'close_patrol_evidence'})
                pinned_preview = {}
            mapping.update(pinned_preview)
            if frames_count == 0:
                actions = [{'kind': 'select_robot', 'target': 'go2'},
                           {'kind': 'camera', 'target': 'Follow robot'},
                           {'kind': 'follow_input', 'zoom_delta': 45},
                           {'kind': 'layer', 'target': 'Room coverage', 'enabled': True},
                           {'kind': 'layer', 'target': 'Localization', 'enabled': True}]
            if at >= duration * .45 and 'lidar' not in sent:
                sent.add('lidar')
                actions += [{'kind': 'layer', 'target': 'Live LiDAR', 'enabled': True}]
            if at >= duration * .7 and 'overview' not in sent:
                sent.add('overview')
                actions += [{'kind': 'camera', 'target': 'Overview'}]
            if milestone == 'interactions' and pending is None:
                action = interaction_action(at, duration, sent, pause_acknowledged_at, snapshot)
                if action:
                    sequence = state['last_command_sequence'] + 1
                    command = {'schema': 'wendy.macos-demo.command.v1', 'run_id': run,
                               'sequence': sequence, 'action': action, 'target': ''}
                    temporary = session / 'commands' / f'.recording-{os.getpid()}.json'
                    write_json(temporary, command)
                    try:
                        os.link(temporary, session / 'commands' / f'{sequence:06d}.json')
                    finally:
                        temporary.unlink()
                    pending = (sequence, action, now, command, observed_speed(snapshot))
                    sent.add(action)
            pointer = None
            if pending:
                sequence, action, sent_at, command, dispatch_speed = pending
                receipt_path = session / 'receipts' / f'{sequence:06d}.json'
                if receipt_path.exists() and state['last_command_sequence'] >= sequence:
                    receipt = read_json(receipt_path, 4096)
                    if not receipt['accepted'] or receipt['action'] != action or receipt['run_id'] != run:
                        raise ValueError('Patrol interaction refused or scope differs')
                    report['commands'].append({'at_s': at, 'command': command, 'receipt': receipt, 'dispatch_speed_mps': dispatch_speed})
                    if action == 'patrol_pause':
                        pause_acknowledged_at = at
                    pointer = {'x': 1720, 'y': 38, 'label': action.removeprefix('patrol_').capitalize() + ' acknowledged'}
                    pending = None
                elif now - sent_at > 5:
                    raise ValueError('Command outcome unknown; no automatic retry')
            if patrol['state'] == 'paused':
                paused_speeds.append(observed_speed(snapshot))
            if 'patrol_resume' in sent and pending is None:
                resumed_speeds.append(observed_speed(snapshot))
            estimate = patrol.get('localization') or {}
            localized += int(estimate.get('valid') is True)
            simulation_times.append(snapshot['time_s'])
            if snapshot['robots']:
                positions.append(snapshot['robots'][0]['position'])
            title = {'walking': 'Go2 in the office', 'localized': 'LiDAR and odometry navigation',
                     'interactions': 'Pause, resume and inspect', 'schedule': 'Room coverage deadlines', 'return': 'Return to the approved rendezvous', 'restrictions': 'Patrol within signed restricted areas'}[milestone]
            detail = 'Articulated body and leg geometry from live MuJoCo joint physics.'
            if milestone == 'restrictions':
                detail = f"Recorded articulated simulation · {patrol['state'].replace('_',' ')} · the same signed areas constrain patrol and return"
            elif milestone == 'return':
                detail = f"Recorded articulated simulation · {patrol['state'].replace('_',' ')} · original mission deadline retained"
            elif milestone == 'schedule':
                detail = 'Signed room intervals · due and overdue coverage · recorded Go2 simulation'
            elif milestone == 'localized':
                detail = 'AMCL estimate and uncertainty are separate from the physical robot.'
            elif milestone == 'interactions':
                detail = f"Mission {patrol['state']}. Coverage and evidence remain visible."
            frame = {'at_s': at, 'state': state, 'assets': mapping, 'actions': actions,
                     'chapter': {'title': title, 'detail': detail, 'series': 'PASSIVE GO2 PATROL'}}
            if pointer:
                frame['pointer'] = pointer
            stream.write(json.dumps(frame, allow_nan=False, separators=(',', ':')) + '\n')
            stream.flush()
            frames_count += 1
            next_frame = started + frames_count / fps
    if pending:
        raise ValueError('Recording ended before command acknowledgement')
    distance = sum(math.dist(a[:2], b[:2]) for a, b in zip(positions, positions[1:]))
    report.update(frames=frames_count, duration_s=duration, sample_fps=fps,
                  observed_ground_truth_distance_m=distance, localization_valid_frames=localized)
    if simulation_times:
        report['simulation_interval_s'] = [simulation_times[0], simulation_times[-1]]
        trace_path = session / 'evaluation.jsonl'
        if trace_path.is_file() and not trace_path.is_symlink() and trace_path.stat().st_size <= 64 << 20:
            samples = []
            for line in trace_path.read_text().splitlines():
                try:
                    sample = json.loads(line)
                except json.JSONDecodeError:
                    continue  # The live writer may still be finishing its last row.
                if simulation_times[0] <= sample.get('time_s', -1) <= simulation_times[-1]:
                    samples.append(sample)
            (out / 'physics-evaluation.jsonl').write_text(''.join(json.dumps(row, allow_nan=False) + '\n' for row in samples))
            if samples and all(len(row.get('joints', [])) == 12 for row in samples):
                report['joint_excursions_rad'] = [max(row['joints'][i] for row in samples) - min(row['joints'][i] for row in samples) for i in range(12)]
                report['foot_contact_samples'] = sum(any(row.get('foot_contacts', [])) for row in samples)
                report['physics_evaluation_samples'] = len(samples)
            errors = sorted(math.hypot(row['truth'][0] - row['estimate']['x'], row['truth'][1] - row['estimate']['y'])
                            for row in samples if row.get('ready') and row.get('estimate'))
            if errors:
                report['sampled_position_error_p95_m'] = errors[math.ceil(.95 * len(errors)) - 1]
                report['sampled_position_error_max_m'] = max(errors)
    if milestone == 'interactions':
        report['minimum_paused_speed_mps'] = min(paused_speeds) if paused_speeds else None
        report['maximum_resumed_speed_mps'] = max(resumed_speeds) if resumed_speeds else None
    write_json(out / 'capture.json', report)
    if frames_count < duration * fps * .5:
        raise ValueError('Too few advancing frames for a live recording')
    if milestone == 'interactions' and ('evidence-open' not in sent or len(report['commands']) != 2):
        raise ValueError('Interaction recording lacks acknowledged pause/resume or captured evidence')
    if milestone == 'interactions' and (not paused_speeds or min(paused_speeds) > .05 or not resumed_speeds or max(resumed_speeds) < .1):
        raise ValueError('Interaction recording did not verify physical stop and resumed travel')
    if milestone in ('walking', 'localized') and distance < .2:
        raise ValueError('Robot did not travel enough to demonstrate walking; raw capture retained')
    if milestone == 'localized' and localized < .95 * frames_count:
        raise ValueError('Localization was unavailable for more than 5% of recording')
    return timeline


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--session', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--milestone', choices=['walking', 'localized', 'interactions', 'schedule', 'return', 'restrictions'], required=True)
    parser.add_argument('--duration', type=float, default=20)
    parser.add_argument('--fps', type=int, default=12)
    parser.add_argument('--app', type=Path, help='integrated Wendy.app or WendyDesktop executable')
    parser.add_argument('--capture-only', action='store_true')
    args = parser.parse_args()
    if not 12 <= args.duration <= 60 or not 1 <= args.fps <= 24:
        parser.error('Duration must be 12–60 seconds, frame rate 1–24')
    out = args.out.resolve()
    if out.is_relative_to(ROOT) or not args.out.is_absolute():
        parser.error('Review media must use an absolute private path outside the repository')
    os.umask(0o077)
    out.mkdir(parents=True, exist_ok=False)
    timeline = capture(args.session.resolve(), out, args.milestone, args.duration, args.fps)
    if not args.capture_only:
        subprocess.run([sys.executable, '-m', 'integration.nativeapp.render',
                        '--timeline', str(timeline), '--out', str(out / 'render'),
                        '--duration', str(args.duration), '--fps', str(args.fps),
                        *(['--app', str(args.app.resolve())] if args.app else [])], cwd=ROOT, check=True)
        source = out / 'render/wendy-world-distributed-fusion.mp4'
        shutil.copyfile(source, out / f'go2-patrol-{args.milestone}.mp4')
    print(out)


if __name__ == '__main__':
    main()
