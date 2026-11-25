#!/usr/bin/env python3
"""Capture real simulator states and controls for the native internal promo."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from sim.collaborative_runtime import SceneRuntime
from sim.macos_demo_bridge import Bridge, SessionFiles, COMMAND_SCHEMA
from sim.office_fusion import OfficeFusion


def write(path, value):
    path.write_text(json.dumps(value, allow_nan=False, indent=2) + '\n')


def capture(scene, coordinator, fusion_binary, out):
    os.umask(0o077)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    timeline = out / 'timeline'
    timeline.mkdir(mode=0o700)
    assets = timeline / 'assets'
    assets.mkdir(mode=0o700)
    run_id = 'office-walkthrough-' + uuid.uuid4().hex[:12]
    runtime = files = fusion = None
    report = {'run_id': run_id, 'kind': 'recorded_simulation_walkthrough', 'hardware': False,
              'commands': [], 'checkpoints': {}, 'frames': 0}
    try:
        runtime = SceneRuntime(scene, coordinator, run_id)
        files = SessionFiles(out, run_id)
        fusion = OfficeFusion(runtime, fusion_binary, out)
        bridge = Bridge(runtime, files, fusion)
        shutil.copyfile(out / 'geometry.json', timeline / 'geometry.json')
        report['worker_pids'] = {node: pipe.process.pid for node, pipe in fusion.pipes.items()}
        if len(set(report['worker_pids'].values())) != 4:
            raise RuntimeError('expected four independent participant processes')
        previous = time.monotonic()

        def tick():
            nonlocal previous
            now = time.monotonic()
            runtime.tick(max(.000001, min(.02, now - previous)))
            previous = now
            fusion.tick()
            if runtime.fault or any(pipe.error for pipe in fusion.pipes.values()):
                raise RuntimeError(runtime.fault or str({n: p.error for n, p in fusion.pipes.items() if p.error}))

        deadline = time.monotonic() + 20
        while True:
            tick()
            snapshots = {n: fusion.snapshot(n) for n in fusion.nodes}
            if (snapshots['hub']['state'] == 'hazard'
                    and all(len(v['observations']) == 7 for v in snapshots.values())):
                report['checkpoints']['connected'] = snapshots
                break
            if time.monotonic() > deadline:
                raise RuntimeError('four local replicas did not receive the initial observations')
            time.sleep(.005)

        def command(action, target=''):
            sequence = bridge.command_sequence + 1
            raw = {'schema': COMMAND_SCHEMA, 'run_id': run_id, 'sequence': sequence,
                   'action': action, 'target': target}
            temporary = out / 'commands' / '.next'
            write(temporary, raw)
            temporary.replace(out / 'commands' / f'{sequence:06d}.json')
            bridge.commands()
            receipt = json.loads((out / 'receipts' / f'{sequence:06d}.json').read_bytes())
            if not receipt['accepted']:
                raise RuntimeError('walkthrough command refused: ' + str(receipt))
            report['commands'].append({'at_s': time.monotonic() - start, **raw, 'receipt': receipt})

        isolated_edges = []
        events_done = set()
        chapter = {'title': 'One site. A shared understanding.',
                   'detail': 'A furnished digital twin, live camera views and LiDAR observations.'}
        start, last_frame = time.monotonic(), -1.
        isolated_position = None
        isolation_verified = False
        rejoin_verified = False
        with (timeline / 'frames.jsonl').open('x') as frames:
            while time.monotonic() - start < 40:
                tick()
                at = time.monotonic() - start
                actions, pointer = [], None
                if 'intro' not in events_done:
                    events_done.add('intro')
                    actions = [{'kind': 'camera', 'target': 'Overview'},
                               {'kind': 'panel', 'target': 'Layers'},
                               {'kind': 'layer', 'target': 'Mesh links', 'enabled': False}]
                if at >= 4 and 'off' not in events_done:
                    events_done.add('off')
                    actions += [{'kind': 'layer', 'target': name, 'enabled': False} for name in ('Observations', 'Hazards')]
                    chapter = {'title': 'Choose what you see.', 'detail': 'Reveal geometry, observations and inferred hazards independently.'}
                if at >= 6 and 'on' not in events_done:
                    events_done.add('on')
                    actions += [{'kind': 'layer', 'target': name, 'enabled': True} for name in ('Observations', 'Hazards')]
                if at >= 9 and 'start' not in events_done:
                    events_done.add('start')
                    command('start')
                    command('select_map', 'atlas')
                    actions += [{'kind': 'select_robot', 'target': 'atlas'},
                                {'kind': 'camera', 'target': 'Follow robot'},
                                {'kind': 'layer', 'target': 'Labels', 'enabled': False},
                                {'kind': 'layer', 'target': 'Live LiDAR', 'enabled': True}]
                    chapter = {'title': 'Follow the robot. Read its world.', 'detail': 'Atlas has its own durable replica and fusion process.'}
                    pointer = {'x': 1780, 'y': 43, 'label': 'Start inspection'}
                if at >= 13 and 'isolate' not in events_done:
                    events_done.add('isolate')
                    for edge_id, edge in runtime.mesh.edges.items():
                        if 'atlas' in (edge['a'], edge['b']) and edge['enabled']:
                            command('toggle_edge', edge_id)
                            isolated_edges.append(edge_id)
                    isolated_position = runtime._pose('atlas')[0]
                    report['checkpoints']['isolation_start'] = {
                        'time_s': float(runtime.data.time), 'position': isolated_position,
                        'fusion': fusion.snapshot('atlas')}
                    actions += [{'kind': 'panel', 'target': 'Network'},
                                {'kind': 'layer', 'target': 'Mesh links', 'enabled': True}]
                    chapter = {'title': 'The link drops. Local work continues.',
                               'detail': 'Atlas follows its cached route and keeps collecting its own LiDAR.'}
                if 'isolate' in events_done and at < 26:
                    view = fusion.snapshot('atlas')
                    if {o['id'] for o in view['observations']} == {'lidar-atlas'}:
                        distance = sum((a-b)**2 for a, b in zip(runtime._pose('atlas')[0], isolated_position))**.5
                        if distance > .1 and view['state'] == 'insufficient_evidence':
                            isolation_verified = True
                            report['checkpoints']['isolated'] = {
                                'time_s': float(runtime.data.time), 'moved_m': distance,
                                'position': runtime._pose('atlas')[0], 'fusion': view,
                                'remote_sources_expired': len(view['missing_observations']) == 6}
                if at >= 19 and 'local' not in events_done:
                    events_done.add('local')
                    actions += [{'kind': 'panel', 'target': 'Layers'}]
                    chapter = {'title': 'Local sensing. Honest uncertainty.',
                               'detail': 'Remote camera, gas and audio observations expire. Atlas keeps its local scan.'}
                if at >= 26 and 'rejoin' not in events_done:
                    events_done.add('rejoin')
                    for edge_id in isolated_edges:
                        command('toggle_edge', edge_id)
                    chapter = {'title': 'Reconnect. Exchange what changed.',
                               'detail': 'Original observations return through the mesh, with their source and capture time.'}
                if 'rejoin' in events_done:
                    view = fusion.snapshot('atlas')
                    if len(view['observations']) == 7 and not view['missing_observations']:
                        rejoin_verified = True
                        report['checkpoints']['rejoined'] = {'time_s': float(runtime.data.time), 'fusion': view}
                if at >= 33 and 'recap' not in events_done:
                    events_done.add('recap')
                    command('select_map', 'hub')
                    actions += [{'kind': 'camera', 'target': 'Overview'},
                                {'kind': 'layer', 'target': 'Live LiDAR', 'enabled': False},
                                {'kind': 'layer', 'target': 'Labels', 'enabled': True}]
                    chapter = {'title': 'Shared knowledge. Independent robots.',
                               'detail': 'Wendy World · Distributed sensor fusion · MuJoCo simulation'}
                if at - last_frame >= .1 or actions or pointer:
                    last_frame = at
                    state = bridge.export()
                    mapping = {}
                    for observation in state['snapshot']['fusion']['observations']:
                        relative = observation.get('evidence_path')
                        if not relative:
                            continue
                        raw = (out / relative).read_bytes()
                        name = hashlib.sha256(raw).hexdigest() + '.png'
                        dest = assets / name
                        if not dest.exists():
                            dest.write_bytes(raw)
                        mapping[relative] = 'assets/' + name
                    frame = {'at_s': at, 'state': state, 'assets': mapping, 'actions': actions, 'chapter': chapter}
                    if pointer:
                        frame['pointer'] = pointer
                    frames.write(json.dumps(frame, allow_nan=False, separators=(',', ':')) + '\n')
                    report['frames'] += 1
                time.sleep(.005)
        report.update(isolated_local_sensing_verified=isolation_verified,
                      original_observations_rejoined=rejoin_verified,
                      simulation_seconds=float(runtime.data.time), wall_seconds=time.monotonic()-start)
        if not isolation_verified or not rejoin_verified:
            raise RuntimeError('walkthrough did not demonstrate isolation and reconciliation')
        report['status'] = 'passed'
    except BaseException as error:
        report.update(status='failed', error=str(error))
        raise
    finally:
        if fusion:
            fusion.close()
        if runtime:
            runtime.stop_mission()
            runtime.close()
        if files:
            files.close()
        report['owned_processes_closed'] = not fusion or all(p.process.poll() is not None for p in fusion.pipes.values())
        write(out / 'walkthrough.json', report)
    print(json.dumps({k: report[k] for k in ('status','frames','simulation_seconds','wall_seconds','owned_processes_closed')}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scene', type=Path, default=ROOT/'examples/collaborative-office/scene.json')
    parser.add_argument('--coordinator', type=Path, required=True)
    parser.add_argument('--fusion', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    capture(args.scene, args.coordinator, args.fusion, args.out)
