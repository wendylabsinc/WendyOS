#!/usr/bin/env python3
"""Generate an exact-target, operator-run published-model camera rehearsal."""
import argparse
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import shlex

ROOT = Path(__file__).resolve().parents[3]
MODEL = 'facebook/detr-resnet-50'
REVISION = '1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b'
SOURCES = ('Examples/WendyDataPeople/campaign.yaml', 'Examples/WendyDataPeople/README.md',
           'go/internal/agent/services/data_inference.go', 'go/internal/agent/inference/worker.py',
           'go/internal/agent/inference/uv.lock', 'go/internal/cli/commands/data.go',
           'go/internal/cli/commands/data_campaign_state.go')


def line(value):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError('nonempty single-line inventory value required')
    return value


def artifact(value):
    path = Path(value).absolute()
    if path.is_symlink() or not path.is_file():
        raise ValueError('regular artifact required: '+str(path))
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for part in iter(lambda: stream.read(1024*1024), b''): digest.update(part)
    return {'path': str(path), 'sha256': digest.hexdigest(), 'size': path.stat().st_size}


def generate(inventory, output):
    raw = json.loads(Path(inventory).read_text())
    def no_secrets(value):
        if isinstance(value, dict):
            for key, item in value.items():
                if any(word in key.lower() for word in ('token', 'password', 'private_key', 'secret')):
                    raise ValueError('credentials must not enter evidence')
                no_secrets(item)
        elif isinstance(value, list):
            for item in value: no_secrets(item)
    no_secrets(raw)
    if not re.fullmatch('[a-f0-9]{32}', raw['trial']): raise ValueError('fresh trial nonce required')
    if not re.fullmatch('[a-f0-9]{40}', raw['source_commit']): raise ValueError('full candidate source commit required')
    if raw.get('reserved_target') is not True or raw.get('no_existing_campaigns_or_recordings') is not True:
        raise ValueError('reserve an idle target, not a production recording device')
    target = raw['target']
    for field in ('serial', 'machine_id', 'os_release', 'kernel', 'owner', 'agent_unit'):
        line(target[field])
    if target['agent_unit'] != 'wendy-agent.service' or target['arch'] not in ('aarch64', 'x86_64'):
        raise ValueError('this procedure requires the supported reserved Linux systemd Agent')
    host, port = raw['endpoint'].rsplit(':', 1)
    address = ipaddress.ip_address(host.strip('[]'))
    if address.is_unspecified or address.is_multicast or not 1 <= int(port) <= 65535:
        raise ValueError('literal selected device endpoint required')
    cameras = raw['cameras']
    if len(cameras) != 2: raise ValueError('two independently identified physical cameras required')
    for camera in cameras:
        for field in ('serial', 'usb_path', 'source_id', 'description'): line(camera[field])
        if not camera['source_id'].startswith('v4l2:'): raise ValueError('physical Linux V4L2 source required')
    for field in ('serial', 'usb_path', 'source_id'):
        if cameras[0][field] == cameras[1][field]: raise ValueError('camera identities must be distinct')
    records = {name: artifact(raw['records'][name]) for name in
               ('authorization', 'device_identity', 'camera_inventory', 'candidate_build',
                'recovery', 'scene_protocol', 'model_manifest')}
    model = json.loads(Path(records['model_manifest']['path']).read_text())
    if model.get('repository') != MODEL or model.get('revision') != REVISION:
        raise ValueError('require the actual example published model/revision')
    files = model.get('files', {})
    if not files or not any(name.endswith('.safetensors') for name in files):
        raise ValueError('reviewed published checkpoint hashes required')
    for name, digest in files.items():
        if not name or Path(name).is_absolute() or '..' in Path(name).parts or not re.fullmatch('[a-f0-9]{64}', digest):
            raise ValueError('invalid checkpoint manifest')
    cli, agent = artifact(raw['cli_binary']), artifact(raw['agent_binary'])
    decoder = artifact(raw['decoder_binary'])
    output = Path(output).absolute()
    if output.exists(): raise ValueError('fresh output required')
    output.mkdir(parents=True, mode=0o700)
    name = 'reg1890-'+raw['trial'][:16]
    campaign = {'version': 1, 'name': name, 'sources': [{'camera': '*', 'capture': {'mode': 'continuous'}}],
                'inference': {'model': MODEL, 'revision': REVISION, 'labels': ['person'], 'threshold': 0.9,
                              'rate': 1, 'accelerator': 'cpu', 'event': 'person_detected',
                              'clear_after': '5s', 'cooldown': '30s', 'enabled': True},
                'capture': {'buffer': '10s', 'after_trigger': '10s', 'triggers': [{'event': 'person_detected'}]},
                'upload': {'when': 'manual'}, 'export': {'annotation': 'cvat'}}
    # JSON is a YAML subset accepted by the production campaign decoder.
    (output/'campaign.yaml').write_text(json.dumps(campaign, indent=2)+'\n')
    profile = output/'cli-profile'
    profile.mkdir(mode=0o700)
    base = ['env', 'WENDY_CONFIG_DIR='+str(profile), 'WENDY_SECRET_STORE=file',
            'WENDY_ANALYTICS=false', cli['path'], '--json', '--device', raw['endpoint']]
    guardian = ROOT/'.github/scripts/run-owned-media-test.py'
    def command(label, args, seconds=45):
        argv = ['python3', str(guardian), '--timeout', str(seconds), '--', *base, *args]
        return shlex.join(argv)+' > '+shlex.quote(str(output/(label+'.json')))+' 2> '+shlex.quote(str(output/(label+'.stderr')))
    commands = {
        'info': command('info', ['device', 'info', '--read-only']),
        'sources': command('sources', ['data', 'sources']),
        'list': command('list', ['data', 'campaign', 'list']),
        'status': command('status', ['data', 'status']),
        'deploy': command('deploy', ['data', 'campaign', 'deploy', str(output/'campaign.yaml'), '--skip-cloud-registration']),
        'inspect': command('inspect', ['data', 'campaign', 'inspect', name]),
        'episodes': command('episodes', ['data', 'episodes']),
        'disarm': command('disarm', ['data', 'campaign', 'disarm', name]),
        'remove': command('remove', ['data', 'campaign', 'remove', name]),
    }
    evidence = {'entry': '1890', 'hardware_execution': 'not_run', 'source_commit': raw['source_commit'],
                'trial': raw['trial'], 'target': target, 'endpoint': raw['endpoint'], 'cameras': cameras,
                'cli': cli, 'agent': agent, 'decoder': decoder, 'records': records, 'model': model, 'campaign': campaign,
                'sources': {name: artifact(ROOT/name)['sha256'] for name in SOURCES},
                'guardian': artifact(guardian), 'commands': commands,
                'cases': [{'name': name, 'status': 'not_run'} for name in
                          ('published_model_physical_detection', 'camera_hotplug_and_cooldown',
                           'agent_restart_restores_plan', 'disarm_remove_releases_capture')],
                'excluded': ['GPU acceleration', 'Cloud delivery', 'accuracy benchmark',
                             'example-app V4L2 sample-ID join', 'certificate/session policy']}
    guide = (Path(__file__).with_suffix('.txt')).read_text()
    for key, value in commands.items(): guide = guide.replace('@'+key.upper()+'@', value)
    guide = guide.replace('@NAME@', name).replace('@PLAN@', str(output)).replace('@CLI@', shlex.join(base[:-3])).replace('@ENDPOINT@', shlex.quote(raw['endpoint']))
    (output/'procedure.txt').write_text(guide)
    evidence['generated'] = {name: artifact(output/name) for name in ('campaign.yaml', 'procedure.txt')}
    evidence['inventory'] = artifact(inventory)
    (output/'evidence.json').write_text(json.dumps(evidence, indent=2)+'\n')
    return evidence


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('inventory', type=Path); parser.add_argument('output', type=Path)
    args = parser.parse_args(); generate(args.inventory, args.output)
