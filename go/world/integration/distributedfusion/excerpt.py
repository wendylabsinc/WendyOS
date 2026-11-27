"""Two completed steps from a failed synthetic run, without a full-run pass claim."""
import argparse
import base64
from copy import deepcopy
from pathlib import Path

from integration.distributedfusion import model, playback
from integration.people.playback import _decoded


def assemble(run_dir, out):
    run_dir, out = Path(run_dir).resolve(), Path(out).absolute()
    model.require(not out.exists(), 'fresh excerpt output required')
    inventory = {}

    def read_bytes(name):
        model.require(isinstance(name, str), 'run-relative receipt required')
        relative = Path(name)
        model.require(not relative.is_absolute() and relative.parts and
                      all(p not in ('', '.', '..') for p in name.split('/')), 'run-relative receipt required')
        path = run_dir / relative
        model.require(not path.is_symlink() and path.is_file() and 0 < path.stat().st_size <= playback.MAX_FILE,
                      'bounded public receipt required')
        raw = path.read_bytes()
        model.require(0 < len(raw) <= playback.MAX_FILE, 'bounded public receipt required')
        inventory[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
        return raw

    def read(name):
        return model.strict_json(read_bytes(name), playback.MAX_FILE)

    result, cleanup = read('result.json'), read('cleanup.json')
    model.require(result.get('passed') is False and result.get('gate_completed') is False and
                  result.get('failed_phase') == 'sample-3' and result.get('simulation_only') is True and
                  cleanup == result.get('cleanup') and cleanup.get('complete') is True and
                  isinstance(cleanup.get('resources'), list) and len(cleanup['resources']) == 7 and
                  all(r.get('after') == 'absent' for r in cleanup['resources']),
                  'failed restoration and completed cleanup required')
    pair, scene, task = read('pair-public.json'), read('scene-identity.json'), read('task-b-reader.json')
    model.validate_pair(pair)
    model.require(all(task.get(k) == v for k, v in {'node': 'b', 'app': model.APP, 'service': 'reader',
                  'device': pair['nodes'][1]['public']['principal']}.items()), 'recorded reader task differs')
    steps = []
    for index in (1, 2):
        # These individual rows were saved only after all receipts/media for
        # that step were collected. No synthetic successful manifest is made.
        row = read(f'steps/{index:04d}.json')
        step = {k: row[k] for k in ('name', 'simulation_time_ns')}
        step.update({k: read(row[k]) for k in ('presentation', 'control', 'reader')})
        step['origins'] = read(row['origin']) if row.get('origin') else {}
        for key in ('gas_origin', 'correspondence'):
            step[key] = read(row[key]) if row.get(key) else None
        hazards = step['reader']['hazards']
        model.require(row.get('hazards') == len(hazards) and row.get('original_envelope_sha256') ==
                      [h['envelope_sha256'] for h in hazards], 'step original set differs')
        sensors = step['control']['request']['sensors']
        model.require(set(row.get('captures', {})) == set(sensors) and
                      set(row.get('media', {})) == set(model.CAMERAS) | {'audio-1'},
                      'retained source evidence set differs')
        step['images'] = {}
        for sensor, name in row['captures'].items():
            capture = read(name)
            original = capture.get('capture', {})
            sample = model.strict_json(_decoded(original.get('sample_json'), 64 << 10), 64 << 10)
            model.require(original.get('scene_boot_id') == scene['boot_id'] and
                          sample.get('sensor_id') == sensor and sample.get('run_id') == scene['run_id'] and
                          sample.get('frame') == scene['frame'] and sample.get('sample_ns') == step['simulation_time_ns'],
                          'retained source sample differs')
            media = base64.b64decode(capture.get('media', ''), validate=True)
            ref = sample.get('evidence')
            if ref:
                raw = read_bytes(row['media'][sensor])
                model.require(raw == media and len(raw) == ref['size'] and model.digest(raw) == ref['sha256'],
                              'retained source media differs')
                if sensor in model.CAMERAS and hazards:
                    step['images'][sensor] = raw
            else:
                model.require(not media, 'inline sample has unexpected media')
        steps.append(step)
    geometry, frames, originals, assets = playback.build_timeline(pair, scene, task['generation'], steps, 30, step_count=2)
    # Restore the gateway explanation after demonstrating the native layer.
    explanation = deepcopy(frames[0])
    explanation.update(at_s=7, actions=[])
    frames.append(explanation)
    frames.sort(key=lambda f: f['at_s'])
    for frame in frames:
        frame['chapter']['series'] = 'RECORDED SIMULATION · TWO-STEP EXCERPT'
        if frame['chapter']['title'] == 'Two Agents. One shared inference.':
            frame['chapter']['title'] = 'A shared hazard across gateways.'
        if frame['source_step']['name'] == playback.NAMES[1]:
            frame['chapter']['detail'] = 'Only Gateway B’s gas stops in this recorded step. A’s sensors stay enabled; B’s hazard result expires.'
    out.mkdir(parents=True, mode=0o700)
    timeline = out / 'timeline'
    (timeline / 'assets').mkdir(parents=True, mode=0o700)
    (timeline / 'geometry.json').write_bytes(model.json_bytes(geometry))
    (timeline / 'frames.jsonl').write_bytes(b''.join(model.json_bytes(frame) for frame in frames))
    for name, raw in assets.items():
        (timeline / name).write_bytes(raw)
    (out / 'originals').mkdir(mode=0o700)
    for digest, raw in originals.items():
        (out / 'originals' / (digest + '.json')).write_bytes(raw)
    report = {'version': 1, 'status': 'excerpt', 'source_run_passed': False,
              'source_failed_phase': 'sample-3', 'full_scenario_incomplete': True,
              'completed_steps': 2, 'restoration_included': False, 'cleanup_complete': True,
              'run_id': scene['run_id'], 'duration_s': 30, 'presentation_frames': len(frames),
              'inputs': inventory, 'gas_source_node': 'b', 'fusion_node': 'a', 'reader_node': 'b',
              'gas_original_bytes_matched_to_fusion_inputs': True,
              'origin_attested_inference': True, 'receiver_raw_inputs_independently_verified': False,
              'new_crypto_verification': False, 'sensor_interpolation': False, 'sample_deadlines_rebased': False,
              'reader_camera_fetches': 0, 'live_commands': 0,
              'scope': 'Native UI excerpt of two completed recorded synthetic steps from an incomplete run',
              'originals': {digest: {'bytes': len(raw), 'sha256': digest} for digest, raw in originals.items()},
              'assets': {name: {'bytes': len(raw), 'sha256': model.digest(raw)} for name, raw in assets.items()}}
    for name in ('geometry.json', 'frames.jsonl'):
        raw = (timeline / name).read_bytes()
        report[name] = {'bytes': len(raw), 'sha256': model.digest(raw)}
    (out / 'excerpt.json').write_bytes(model.json_bytes(report))
    (out / 'README.md').write_text(
        '# Recorded distributed fusion excerpt\n\n'
        'The source run failed during sample 3. This is a 30-second presentation of its two completed steps, '
        'not a full scenario pass or a restoration demonstration. Cleanup completed.\n\n'
        'Gateway B published gas. Gateway A fused that exact original with its local camera/audio/LiDAR '
        'observations, and B read the exact A-signed hazard. Then only B gas was disabled; fresh A sources '
        'remained enabled and B reported no live hazard after both original gas and result deadlines. '
        'Absence is not clearance. Camera images are retained A-origin evidence, not B camera reads.\n\n'
        'The adapter checks exact receipt correspondence without new cryptographic verification. '
        'Sample times and deadlines are unchanged. Video holds recorded states; camera presets and '
        'Hazards toggles are real native UI presentation actions. No live simulation controls are sent.\n')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    report = assemble(args.run_dir, args.out)
    print(model.json_bytes({k: report[k] for k in ('status', 'source_run_passed', 'completed_steps',
                                                'full_scenario_incomplete')}).decode(), end='')


if __name__ == '__main__':
    main()
