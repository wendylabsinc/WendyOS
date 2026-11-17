#!/usr/bin/env python3
"""Generate reserved-account macOS Agent lifecycle acceptance; never install or run."""
import argparse
import hashlib
import importlib.util
import ipaddress
import json
from pathlib import Path
import plistlib
import re
import shlex
import zipfile

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location('mac_lifecycle_records', Path(__file__).with_name('mac-camera-rehearsal.py'))
COMMON = importlib.util.module_from_spec(spec)
spec.loader.exec_module(COMMON)
LABEL = 'sh.wendy.WendyAgentMac'
SOURCES = ('swift/WendyAgentMac/Sources/LaunchAgentSupervision.swift',
           'swift/WendyAgentMac/Sources/AppDelegate.swift',
           'swift/WendyAgentMac/LaunchAgents/sh.wendy.WendyAgentMac.plist',
           'swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/AgentRelauncher.swift',
           'swift/WendyAgentCore/Sources/WendyAgent/Services/AgentUpdateSession.swift',
           'go/internal/cli/commands/device.go', 'swift/Scripts/Build.sh', 'swift/Makefile')


def bundle(value, archive):
    record = COMMON.bundle_inventory(value)
    root = Path(record['path'])
    info = plistlib.loads((root/'Contents/Info.plist').read_bytes())
    if info.get('CFBundleIdentifier') != LABEL or info.get('CFBundleExecutable') != 'WendyAgentMac':
        raise ValueError('require the actual Wendy Agent Mac bundle identity')
    COMMON.line(info.get('WLWendyAgentVersion'), 'WLWendyAgentVersion')
    binary = COMMON.artifact(root/'Contents/MacOS/WendyAgentMac')
    packaged = COMMON.artifact(archive)
    # Bind the update ZIP to the inventoried bundle's executable and metadata.
    # Full signature/resource validation is separately mandatory on the Mac.
    with zipfile.ZipFile(packaged['path']) as source:
        names = source.namelist()
        if len(names) > 100000 or len(names) != len(set(names)):
            raise ValueError('ambiguous or oversized app archive')
        top = {name.split('/')[0] for name in names if name.endswith('/Contents/Info.plist')}
        if len(top) != 1:
            raise ValueError('require one top-level app in the ZIP')
        prefix = top.pop()
        if not prefix.endswith('.app'):
            raise ValueError('archive root is not an app bundle')
        for relative in ('Contents/Info.plist', 'Contents/MacOS/WendyAgentMac',
                         'Contents/Library/LaunchAgents/'+LABEL+'.plist'):
            entry = source.getinfo(prefix+'/'+relative)
            if entry.file_size > 512*1024**2:
                raise ValueError('oversized lifecycle archive member')
            digest = hashlib.sha256()
            with source.open(entry) as stream:
                for chunk in iter(lambda: stream.read(1024*1024), b''): digest.update(chunk)
            if digest.hexdigest() != COMMON.artifact(root/relative)['sha256']:
                raise ValueError('ZIP and inventoried app differ: '+relative)
    launch = plistlib.loads((root/'Contents/Library/LaunchAgents'/f'{LABEL}.plist').read_bytes())
    if launch.get('Label') != LABEL or launch.get('BundleProgram') != 'Contents/MacOS/WendyAgentMac':
        raise ValueError('wrong embedded launch agent')
    return {'bundle': record, 'archive': packaged, 'executable': binary,
            'version': info['WLWendyAgentVersion'], 'launch_agent': launch}


def generate(inventory, output):
    raw = json.loads(Path(inventory).read_text())
    def reject_secrets(value):
        if isinstance(value, dict):
            for key, child in value.items():
                if any(part in key.lower() for part in ('password', 'private_key', 'token', 'passphrase')):
                    raise ValueError('do not record credentials in the lifecycle inventory')
                reject_secrets(child)
        elif isinstance(value, list):
            for child in value: reject_secrets(child)
    reject_secrets(raw)
    if not re.fullmatch('[a-f0-9]{32}', raw['trial']): raise ValueError('fresh trial nonce required')
    for name in ('source_commit', 'origin_source_commit'):
        if not re.fullmatch('[a-f0-9]{40}', raw[name]): raise ValueError('full source revisions required')
    host = raw['mac']
    for name in ('serial', 'hardware_uuid', 'os_build', 'owner', 'account'):
        COMMON.line(host[name], name)
    if not re.fullmatch('[a-z][a-z0-9_]{1,30}', host['account']) or type(host['uid']) is not int or host['uid'] < 501:
        raise ValueError('require a named disposable ordinary Mac account')
    if raw.get('reserved_account_empty') is not True:
        raise ValueError('existing installed Agent/user state must not be used')
    installed = Path(raw['installed_bundle'])
    account_home = Path('/Users')/host['account']
    if installed != account_home/'Applications/WendyAgentMac.app':
        raise ValueError('installation must stay in the reserved account Applications directory')
    endpoint_host, port = raw['endpoint'].rsplit(':', 1)
    if not ipaddress.ip_address(endpoint_host).is_loopback or not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise ValueError('use the reserved Mac local Agent endpoint')
    candidate = bundle(raw['candidate_bundle'], raw['candidate_zip'])
    origin = bundle(raw['origin_bundle'], raw['origin_zip'])
    if candidate['executable']['sha256'] == origin['executable']['sha256']:
        raise ValueError('origin and candidate must have different executable bytes')
    expected = candidate['launch_agent']
    if expected.get('KeepAlive') != {'SuccessfulExit': False} or expected.get('AbandonProcessGroup') is not True or expected.get('EnvironmentVariables', {}).get('WENDY_AGENT_LAUNCHD') != '1':
        raise ValueError('candidate must contain the actual supervision contract')
    records = {name: COMMON.artifact(raw['records'][name]) for name in
               ('identity', 'empty_account', 'authorization', 'signing', 'origin_build', 'candidate_build', 'recovery')}
    cli = COMMON.artifact(raw['cli_binary'])
    enrollment = None
    if 'enrollment_peer_binary' in raw:
        if endpoint_host != '127.0.0.1' or int(port) == 65535:
            raise ValueError('packaged enrollment requires IPv4 loopback setup and adjacent mTLS port')
        enrollment = {'peer': COMMON.artifact(raw['enrollment_peer_binary']),
                      'build_record': COMMON.artifact(raw['records']['enrollment_peer_build'])}
    out = COMMON.new_output(output)
    profile = out/'cli-config'; profile.mkdir(mode=0o700)
    app_id = 'sh.wendy.tests.lifecycle.'+raw['trial']
    project = out/'heartbeat'; project.mkdir(mode=0o700)
    receipt = out/'heartbeat.log'
    # Only the unique fixture's native process writes this owned transcript.
    # Its PID and continuously increasing counter expose teardown/relaunch.
    script = '#!/bin/sh\nset -eu\ni=0\nwhile [ "$i" -lt 900 ]; do\n printf "%s %s %s\\n" '+shlex.quote(raw['trial'])+' "$$" "$i" >> '+shlex.quote(str(receipt))+'\n i=$((i+1))\n /bin/sleep 1\ndone\n'
    (project/'launch.sh').write_text(script); (project/'launch.sh').chmod(0o700)
    (project/'wendy.json').write_text(json.dumps({'appId': app_id, 'platform': 'darwin', 'run': {'command': 'launch.sh'}}, indent=2)+'\n')
    env = ['env', 'WENDY_CONFIG_DIR='+str(profile), 'WENDY_SECRET_STORE=file', 'WENDY_ANALYTICS=false', 'WENDY_NO_UPDATE_CHECK=1']
    base = env+[cli['path'], '--device', raw['endpoint']]
    domain = f"gui/{host['uid']}/{LABEL}"
    commands = {
        'job': ['/bin/launchctl', 'print', domain],
        'crash-owned-agent': ['/bin/launchctl', 'kill', 'SIGABRT', domain],
        'open-installed': ['/usr/bin/open', '-n', str(installed)],
        'info': base+['--json', 'device', 'info'],
        'deploy-heartbeat': base+['--json=false', 'run', '--prefix', str(project), '--detach', '--skip-cloud-registration'],
        'update': base+['--json', 'device', 'update', '--binary', candidate['archive']['path']],
        'restore-origin': base+['--json', 'device', 'update', '--binary', origin['archive']['path']],
        'apps': base+['--json', 'device', 'apps', 'list'],
        'remove-heartbeat': base+['device', 'apps', 'remove', app_id, '--force'],
    }
    enrollment_outputs = []
    if enrollment:
        (out/'enrollment-home').mkdir(mode=0o700)
        action = out/'enrollment-actions.py'
        action.write_bytes(Path(__file__).with_name('mac-agent-enrollment.py').read_bytes())
        settings = out/'enrollment-plan.json'
        settings.write_text(json.dumps({'mac': host, 'cli': cli, 'peer': enrollment['peer'],
            'candidate': candidate['executable'], 'installed': str(installed), 'port': int(port),
            'output': str(out), 'source_commit': raw['source_commit'],
            'peer_build_record': enrollment['build_record']}, indent=2)+'\n')
        for verb in ('peer', 'enroll', 'apps', 'status'):
            commands['enrollment-'+verb] = ['python3', str(action), str(settings), verb]
        enrollment_outputs = [action, settings]
    # Reviewed source overlays for separately built/signed controls. They never
    # change the source checkout or the code seal of an existing package.
    control = out/'controls'; control.mkdir(mode=0o700)
    app_source = (ROOT/SOURCES[1]).read_text()
    before = 'await self?.performUpdateQuit()'
    if app_source.count(before) != 1: raise ValueError('update-specific quit source site changed')
    (control/'AppDelegate.swift').write_text(app_source.replace(before, 'await self?.performQuit()'))
    plist = plistlib.loads((ROOT/SOURCES[2]).read_bytes())
    plist['KeepAlive'] = False
    plist['RunAtLoad'] = True
    (control/(LABEL+'.plist')).write_bytes(plistlib.dumps(plist))
    evidence = {'audit_entries': ['2314', '1786'], 'hardware_execution': 'not_run', 'inventory': raw,
                'inventory_record': COMMON.artifact(inventory), 'candidate': candidate, 'origin': origin,
                'cli': cli, 'records': records, 'app_id': app_id,
                'sources': [COMMON.artifact(ROOT/name) for name in SOURCES]+
                           [COMMON.artifact(p) for p in (Path(__file__), Path(__file__).with_suffix('.txt'),
                            Path(__file__).with_name('mac-camera-rehearsal.py'), ROOT/'.github/scripts/run-owned-media-test.py')],
                'cases': [{'id': name, 'status': 'not_run', 'observations': []} for name in
                          ('signed-install-handoff', 'crash-restart', 'clean-exit-stays-stopped',
                           'logout-login', 'host-reboot-login', 'package-update-new-process',
                           'native-app-survives-update', 'disabled-setting', 'historical-controls', 'restoration')]}
    if enrollment:
        evidence['audit_entries'].append('2318')
        evidence['enrollment_peer'] = enrollment
        evidence['sources'] += [COMMON.artifact(Path(__file__).with_name('mac-agent-enrollment.py')),
            COMMON.artifact(ROOT/'go/internal/testfixture/macenrollment/main.go')]
        evidence['cases'] += [{'id': name, 'status': 'not_run', 'observations': []} for name in
            ('signed-package-enrollment', 'signed-package-enrollment-restart')]
    (out/'commands.json').write_text(json.dumps(commands, indent=2)+'\n')
    guide = Path(__file__).with_suffix('.txt').read_text()
    for key, value in {'@TRIAL@': raw['trial'], '@UID@': str(host['uid']), '@ACCOUNT@': host['account'],
                       '@UUID@': host['hardware_uuid'], '@SERIAL@': host['serial'], '@INSTALLED@': str(installed),
                       '@OUT@': str(out), '@APPID@': app_id, '@CANDIDATE@': candidate['executable']['sha256'],
                       '@ORIGIN@': origin['executable']['sha256'], '@CANDIDATE_APP@': candidate['bundle']['path']}.items(): guide = guide.replace(key, value)
    guide += '\nExact commands (each is an explicit human action, never executed by generation):\n'
    guardian = ROOT/'.github/scripts/run-owned-media-test.py'
    for name, command in commands.items():
        duration = 620 if name == 'enrollment-peer' else 120 if name == 'enrollment-enroll' else 180 if name in ('update', 'restore-origin', 'deploy-heartbeat') else 15
        guide += '\n'+name+':\n'+shlex.join(['python3', str(guardian), '--timeout', str(duration), '--', *command])+'\n'
    (out/'procedure.txt').write_text(guide)
    evidence['generated'] = [COMMON.artifact(path) for path in
                             (out/'commands.json', out/'procedure.txt', project/'launch.sh', project/'wendy.json',
                              control/'AppDelegate.swift', control/(LABEL+'.plist'))]
    evidence['generated'] += [COMMON.artifact(path) for path in enrollment_outputs]
    (out/'evidence.json').write_text(json.dumps(evidence, indent=2)+'\n')
    return evidence


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('inventory', type=Path); parser.add_argument('output', type=Path)
    args = parser.parse_args(); generate(args.inventory, args.output)
