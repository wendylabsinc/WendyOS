#!/usr/bin/env python3
"""Owned local-process signed inspection, using existing production executables.

The selected inference is computed from real owned MuJoCo captures. Its origin
wrapper is explicitly a local fixture, never an enrolled native Agent record.
"""
from __future__ import annotations
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from sim.macos_demo_bridge import SessionFiles
from integration.hazard.playback import fusion_view
from integration.people.playback import timestamp_ns

ANCHOR = 1767225600000000000
SENSORS = ['camera-a', 'camera-b', 'gas-1', 'audio-1', 'lidar-atlas']



def write(path, value):
    raw = value if isinstance(value, bytes) else json.dumps(value, separators=(',', ':'), allow_nan=False).encode() + b'\n'
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as f: f.write(raw)


def atomic(path, value):
    tmp = path.with_name('.' + path.name + secrets.token_hex(4))
    write(tmp, value)
    os.replace(tmp, path)


def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()


def validate_mode(mode, ui_command):
    if mode == 'auto' and ui_command:
        raise ValueError('auto and native UI signing are separate walkthrough modes')


def selected_task(task, handoff, task_id):
    """Bind a completion to the exact reviewed original and named submission."""
    if not task_id or task.get('spec', {}).get('id') != task_id:
        return False
    spec = task['spec']
    trigger = json.loads(base64.b64decode(handoff['mission_input']['trigger_json'], validate=True))
    return (spec.get('proposal', {}).get('id') == handoff['proposal_id'] and
            spec.get('trigger') == trigger and trigger['sha256'] == handoff['mission_input']['trigger_sha256'] and
            spec.get('target') == handoff['navigation_target'] and
            spec.get('inspection_destination') == handoff['inspection_destination'])


def expected_task_id(directory, mode, run_id, handoff):
    if mode == 'auto':
        return json.loads((directory / 'accepted-task.json').read_text())['spec']['id']
    path = directory / 'native-ui/result.json'
    if not path.exists():
        return None
    receipt = json.loads(path.read_text())
    if (receipt.get('status') != 'passed' or receipt.get('run_id') != run_id or
            receipt.get('selected_original') != handoff['original_envelope_sha256']):
        raise ValueError('native UI result does not match the selected original')
    return receipt['task_id']


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def http(url, path, token, value=None, timeout=3):
    body = None if value is None else json.dumps(value, separators=(',', ':')).encode()
    request = urllib.request.Request(url + path, data=body, headers={'Authorization': 'Bearer ' + token,
                                                                   'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        raw = response.read((4 << 20) + 1)
        if len(raw) > 4 << 20: raise ValueError('source response too large')
        return raw


def relocate_gateway(config, directory, listen):
    """Change only local transport/storage and enable the existing signed status export."""
    value = json.loads(json.dumps(config))
    for key in ('data_path', 'proof_path', 'replay_path', 'revocation_path'):
        value[key] = str(directory / Path(value[key]).name)
    for key in ('tasks', 'proposals', 'evidence'):
        value[key]['path'] = str(directory / Path(value[key]['path']).name)
    value['listen'] = listen
    value['task_relay'] = {'path': str(directory / 'task-relay.json'), 'max_entries': 64, 'max_bytes': 4 << 20,
        'routes': {value['audience']: {'assignee': value['tasks']['assignee'],
                                      'status_principal': value['peer_grant']['grant']['principal']}}}
    return value


def display_state(run_id, sequence, presentation, hazard, original, sample_ns, phase, *, observation_sample_ns):
    live = hazard is not None and sample_ns < timestamp_ns(hazard['valid_until']) - ANCHOR
    view = fusion_view(hazard if live else None, sample_ns,
                       {'camera-a': True, 'camera-b': True} if live else {})
    if live:
        view.update(original_envelope_sha256=original,
                    sample_time_s=observation_sample_ns / 1e9,
                    reason='Camera, LiDAR, gas and audio support this location.',
                    basis='LOCAL SIMULATION · Actual inference from owned captures; local fixture origin.')
    else:
        view.update(reason='The selected observation has expired. It remains available in the mission review.',
                    basis='LOCAL SIMULATION · Expiry is not clearance.')
    return {'schema': 'wendy.macos-demo.state.v1', 'mode': 'local-inspection', 'run_id': run_id,
        'sequence': sequence, 'worker_status': 'running', 'last_command_sequence': 0,
        'snapshot': {'run_id': run_id, 'time_s': sample_ns / 1e9, 'phase': phase, 'authorized': False,
                     'stopped': False, 'robots': [], 'jobs': [], 'completed': 0, 'total': 0, 'events': [],
                     'voxels': [], 'voxel_size': .2, 'map_node': 'local-gateway', 'fusion': view,
                     'controls_available': False, 'sensor_sources': presentation.get('sensor_sources', []),
                     'network': {'edges': [], 'internet': {'id': 'internet', 'a': 'local-gateway', 'b': 'internet',
                         'enabled': False, 'up': False, 'packets': 0}, 'gossip_rounds': 0, 'peer_voxels': {}}},
        'dynamic_geoms': presentation['dynamic_geoms']}


class OwnedRun:
    def __init__(self, output, driver, binaries, python=sys.executable):
        self.out, self.driver, self.binaries = output, driver, binaries
        self.python = python
        self.processes = []
        self.logs = []
        self.stop = False

    def start(self, name, argv, **kwargs):
        stdout = open(self.out / (name + '.stdout'), 'xb', buffering=0)
        stderr = open(self.out / (name + '.stderr'), 'xb', buffering=0)
        self.logs += [stdout, stderr]
        process = subprocess.Popen([str(x) for x in argv], cwd=ROOT, stdout=stdout, stderr=stderr,
                                   start_new_session=True, **kwargs)
        self.processes.append((name, process))
        return process

    def command(self, name, argv, timeout=15):
        with open(self.out / (name + '.stdout'), 'xb') as stdout, open(self.out / (name + '.stderr'), 'xb') as stderr:
            result = subprocess.run([str(x) for x in argv], cwd=ROOT, stdout=stdout, stderr=stderr, timeout=timeout)
        if result.returncode: raise RuntimeError(f'{name} failed ({result.returncode}); see retained stderr')

    def close(self):
        rows = []
        for name, p in reversed(self.processes):
            if p.poll() is None:
                os.killpg(p.pid, signal.SIGTERM)
                try: p.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(p.pid, signal.SIGKILL); p.wait(timeout=3)
            rows.append({'name': name, 'pid': p.pid, 'exit_code': p.returncode, 'owned_process_exited': True})
        for f in self.logs: f.close()
        write(self.out / 'cleanup.json', {'version': 1, 'processes': rows, 'complete': True})

    def execute(self, mode, run_id, ui_command=None, max_seconds=100):
        validate_mode(mode, ui_command)
        out = self.out
        read_token, control_token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
        write(out / 'scene-read-token', read_token.encode()); write(out / 'scene-control-token', control_token.encode())
        source_port, gateway_port = port(), port()
        source_url, gateway_url = f'http://127.0.0.1:{source_port}', f'http://127.0.0.1:{gateway_port}'
        self.start('scene', [self.python, '-m', 'integration.sharedperception.scene', '--inspection',
            '--inspection-defer-start', '--inspection-fixed-capture-hz', '1', '--bind', '127.0.0.1',
            '--port', str(source_port), '--run-id', run_id, '--anchor-unix-ns', str(ANCHOR),
            '--read-token-file', out / 'scene-read-token', '--control-token-file', out / 'scene-control-token'])
        identity = None
        for _ in range(150):
            try:
                identity = json.loads(http(source_url, '/v1/identity', read_token)); break
            except (OSError, ValueError): time.sleep(.1)
        if identity is None: raise RuntimeError('owned scene did not become ready')
        presentation = json.loads(http(source_url, '/v1/presentation', read_token))
        write(out / 'identity.json', identity); write(out / 'initial-presentation.json', presentation)
        tenant = '11111111-1111-4111-8111-111111111111'
        device = 'spiffe://wendy.sh/tenant/' + tenant + '/device/22222222-2222-4222-8222-222222222222'
        office = ROOT / 'examples/collaborative-office/scene.json'
        commission = {'version': 1, 'tenant': tenant, 'site': 'studio-01', 'device': device,
            'run_id': run_id, 'frame': identity['frame'], 'package_sha256': identity['package_sha256'],
            'model_sha256': presentation['geometry']['model_sha256'], 'office_scene_sha256': digest(office),
            'scene_url': source_url, 'gateway_url': gateway_url, 'operator_url': gateway_url}
        write(out / 'commission-input.json', commission)
        self.command('commission', [self.binaries / 'mission-commissioner', 'commission', '--input',
            out / 'commission-input.json', '--office-scene', office, '--out', out / 'commissioned'])
        state_dir = out / 'gateway-state'; state_dir.mkdir(mode=0o700)
        cfg = json.loads((out / 'commissioned/gateway.json').read_text())
        write(out / 'gateway-local.json', relocate_gateway(cfg, state_dir, f'127.0.0.1:{gateway_port}'))
        self.start('gateway', [self.binaries / 'mission-gateway', '--config', out / 'gateway-local.json'])
        for n in range(30):
            result = subprocess.run([str(self.binaries / 'mission-commissioner'), 'catalog', '--credentials',
                                     str(out / 'commissioned/executor.json')], capture_output=True)
            if result.returncode == 0:
                write(out / 'catalog.json', result.stdout); break
            time.sleep(.1)
        else: raise RuntimeError('signed gateway catalog unavailable')
        # Preserve the deferred-start ordering: no deployment time consumes physics.
        started = time.monotonic()
        step = json.loads(http(source_url, '/v1/step', control_token,
                          {'simulation_time_ns': 1_000_000_000, 'sensors': SENSORS}, timeout=6))
        write(out / 'source-start.json', step)
        self.start('executor', [self.binaries / 'atlas-executor', '--credentials', out / 'commissioned/executor.json',
            '--adapter', source_url, '--adapter-token-file', out / 'scene-control-token',
            '--adapter-read-token-file', out / 'scene-read-token', '--run-id', run_id,
            '--frame', identity['frame']['id'], '--revision', identity['frame']['revision'],
            '--planar-config', out / 'commissioned/planar.json'])
        captures = None
        for _ in range(15):
            values = {sensor: http(source_url, '/v1/captures/' + sensor, read_token) for sensor in SENSORS}
            samples = [json.loads(base64.b64decode(json.loads(raw)['capture']['sample_json'])) for raw in values.values()]
            if len({s['sample_ns'] for s in samples}) == 1:
                captures = values; break
            time.sleep(.05)
        if captures is None: raise RuntimeError('coherent fixed capture batch unavailable')
        (out / 'captures').mkdir(mode=0o700)
        for sensor, raw in captures.items(): write(out / 'captures' / (sensor + '.json'), raw)
        write(out / 'observation-config.json', {'Tenant':tenant, 'Site':'studio-01', 'Device':device,
                                               'RunID':run_id, 'Frame':identity['frame']})
        self.command('observe', [self.driver, 'observe', out])
        self.command('propose', [self.binaries / 'mission-commissioner', 'propose', '--credentials',
            out / 'commissioned/proposer.json', '--mission-input', out / 'mission-input.json',
            '--handoff', out / 'commissioned/handoff.json', '--id', 'observed-local-hazard',
            '--navigation-directory', out / 'commissioned', '--source-read-token-file', out / 'scene-read-token'])
        hazard = json.loads((out / 'hazard.json').read_text())
        handoff = json.loads((out / 'commissioned/handoff.json').read_text())
        trigger = json.loads(base64.b64decode(handoff['mission_input']['trigger_json'], validate=True))
        observation_sample_ns = timestamp_ns(trigger['component']['timing']['sample_time']) - ANCHOR
        session = out / 'session'; session.mkdir(mode=0o700)
        files = SessionFiles(session, run_id)
        (session / 'fusion/origin').mkdir(parents=True, mode=0o700)
        for sensor in ['camera-a', 'camera-b']:
            write(session / 'fusion/origin' / (sensor + '.png'), base64.b64decode(json.loads(captures[sensor])['media']))
        presentation = json.loads(http(source_url, '/v1/presentation', read_token))
        files.write('geometry.json', presentation['geometry'])
        sequence = 1
        state = display_state(run_id, sequence, presentation, hazard,
                              handoff['original_envelope_sha256'], presentation['simulation_time_ns'], 'inspection',
                              observation_sample_ns=observation_sample_ns)
        files.write('state.json', state)
        ready = {'version':1, 'run_id':run_id, 'pid':os.getpid(), 'session':str(session),
            'ui_config':str(out / 'commissioned/ui.json'), 'handoff':str(out / 'commissioned/handoff.json'),
            'source_url':source_url, 'gateway_url':gateway_url, 'finish_handle':str(out / 'finish'),
            'remaining_wall_budget_s':max_seconds - (time.monotonic()-started),
            'source_simulation_time_s':presentation['simulation_time_ns']/1e9,
            'source_simulation_limit_s':120, 'native_agent_origin_verified':False,
            'selected_original_envelope_sha256':handoff['original_envelope_sha256']}
        write(out / 'ready.json', ready); print(json.dumps(ready), flush=True)
        ui_process = None
        if ui_command:
            ui_out = out / 'native-ui'
            ui_process = self.start('native-ui', [*ui_command, session, out / 'commissioned/ui.json', ui_out, run_id])
        if mode == 'auto': self.command('authorize', [self.driver, 'authorize', out])
        last_status = None
        task_succeeded = False
        with open(out / 'timeline.jsonl', 'xb') as timeline:
            try:
                while not self.stop and time.monotonic()-started < max_seconds and not (out/'finish').exists():
                    tick = time.monotonic()
                    presentation = json.loads(http(source_url, '/v1/presentation', read_token))
                    robot = json.loads(http(source_url, '/v1/inspection/state', read_token))
                    sequence += 1
                    state = display_state(run_id, sequence, presentation, hazard,
                        handoff['original_envelope_sha256'], presentation['simulation_time_ns'], 'inspection',
                        observation_sample_ns=observation_sample_ns)
                    files.write('state.json', state, replace=True)
                    row = {'at_s':time.monotonic()-started, 'state':state, 'actuator':robot}
                    if sequence % 4 == 0:
                        response = subprocess.run([str(self.driver), 'status', str(out)], capture_output=True, timeout=4)
                        if response.returncode: raise RuntimeError('signed task status failed: '+response.stderr.decode()[:256])
                        last_status = json.loads(response.stdout); row['task_status'] = last_status
                    expected = expected_task_id(out, mode, run_id, handoff)
                    if expected and last_status is not None:
                        for task in last_status['tasks']:
                            if task.get('spec', {}).get('id') != expected:
                                continue
                            if not selected_task(task, handoff, expected):
                                raise RuntimeError('named task differs from the selected handoff')
                            if task['status']['Phase'] == 'succeeded': task_succeeded = True
                            elif task['status']['Phase'] in ['failed','stopped','rejected']:
                                raise RuntimeError('selected task terminal '+task['status']['Phase'])
                    timeline.write(json.dumps(row,separators=(',',':')).encode()+b'\n'); timeline.flush()
                    if task_succeeded and mode == 'auto': break
                    if ui_process is not None and ui_process.poll() is not None:
                        if ui_process.returncode != 0:
                            raise RuntimeError('native UI capture failed; see its retained receipt')
                        break
                    time.sleep(max(0,.2-(time.monotonic()-tick)))
                if task_succeeded:
                    # Existing worldd status publisher runs separately; wait only for its signature.
                    for attempt in range(12):
                        response = subprocess.run([str(self.driver),'fetch',str(out),expected],capture_output=True,timeout=15)
                        if response.returncode == 0: break
                        if b'not yet available' not in response.stderr: raise RuntimeError(response.stderr.decode()[:512])
                        time.sleep(.1)
                    else: raise RuntimeError('terminal gateway status signature unavailable')
                write(out / 'walkthrough.json', {'version':1,'status':'complete' if task_succeeded else 'incomplete',
                    'signed_task_succeeded':task_succeeded,'native_agent_origin_verified':False,
                    'actual_existing_executor':True,'source_simulation_limit_s':120,'last_status':last_status,
                    'elapsed_s':time.monotonic()-started,'timeline_frames':sequence-1})
            finally: files.close()
        return task_succeeded


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--driver',type=Path,required=True)
    parser.add_argument('--binaries',type=Path,required=True,help='Directory produced by integration.localinspection.build')
    parser.add_argument('--python',default=sys.executable,help='Python with MuJoCo, NumPy and Pillow installed')
    parser.add_argument('--mode',choices=['auto','ready'],default='auto')
    parser.add_argument('--run-id',required=True)
    parser.add_argument('--max-seconds',type=int,default=100)
    parser.add_argument('--ui-command',nargs='+')
    args=parser.parse_args()
    try: validate_mode(args.mode, args.ui_command)
    except ValueError as exc: parser.error(str(exc))
    output=args.output.absolute()
    if output.exists() or not str(output).startswith('/tmp/wendy-world-review-localinspection-') or not 15<=args.max_seconds<=105:
        parser.error('fresh private /tmp/wendy-world-review-localinspection-* output and bounded duration required')
    output.mkdir(mode=0o700)
    run=OwnedRun(output,args.driver.resolve(),args.binaries.resolve(),args.python)
    def stopped(*_): run.stop=True
    signal.signal(signal.SIGTERM,stopped);signal.signal(signal.SIGINT,stopped)
    try:
        if not run.execute(args.mode,args.run_id,args.ui_command,args.max_seconds): sys.exit(2)
    except Exception as exc:
        write(output/'failure.json',{'version':1,'error':str(exc),'status':'failed'})
        raise
    finally: run.close()

if __name__=='__main__': main()
