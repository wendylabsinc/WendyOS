#!/usr/bin/env python3
"""Commission ordinary signed mission services beside shared perception apps."""
import argparse
import ipaddress
from pathlib import Path
import re

from integration.remotemediafusion import libraries
from integration.sharedinspection import model, stage

control = libraries.load('sharedinspection_control', libraries.HERE / 'control.py',
                         {'libraries': libraries, 'model': model})
control.VOLUME_DIRECTORIES = control.VOLUME_DIRECTORIES.replace('world-remotemediafusion-', 'world-sharedinspection-')
_volume_count = 'assert len(names)==2 and len(set(names))==2'
_volume_kinds = "zip(names,('config','proofs'))"
model.require(control.VOLUME_DIRECTORIES.count(_volume_count) == 1 and
              control.VOLUME_DIRECTORIES.count(_volume_kinds) == 1, 'known inherited volume guard required')
control.VOLUME_DIRECTORIES = control.VOLUME_DIRECTORIES.replace(_volume_count,
    "assert len(names) in (2,3) and len(set(names))==len(names)\n"
    " assert len(names)==2 or names[0].endswith('-a-mission-gateway-config')").replace(
        _volume_kinds, "zip(names,('config','proofs','state')[:len(names)])")


def volume_receipt(raw, names):
    """Only Atlas's gateway adds the existing mission store's third volume."""
    model.require(isinstance(names, list) and len(names) in (2, 3) and len(set(names)) == len(names) and
        all(isinstance(name, str) and re.fullmatch(r'world-sharedinspection-[a-z0-9-]{1,100}-' + kind, name)
            for name, kind in zip(names, ('config', 'proofs', 'state'))) and
        len({name.rsplit('-', 1)[0] for name in names}) == 1 and
        (len(names) == 2 or names[0].endswith('-a-mission-gateway-config')), 'exact service volume names required')
    value = model.strict_json(raw, 4096)
    model.require(isinstance(value, dict) and set(value) == {'version', 'directories'} and
        type(value['version']) is int and value['version'] == 1 and isinstance(value['directories'], list) and
        len(value['directories']) == 2 + len(names), 'bounded volume directory receipt required')
    for index, (row, name) in enumerate(zip(value['directories'], ['wendy', 'volumes', *names])):
        model.require(isinstance(row, dict) and set(row) == {'component', 'uid', 'mode', 'created'} and
            row['component'] == name and type(row['uid']) is int and row['uid'] == 0 and
            type(row['mode']) is int and 0 <= row['mode'] <= 0o777 and not row['mode'] & 0o022 and
            type(row['created']) is bool, 'protected volume metadata required')
        if index >= 2 or row['created']:
            model.require(row['mode'] == 0o700 and row['created'], 'fresh private volume required')
    return value


control.volume_receipt = volume_receipt
base = libraries.load('sharedinspection_remote_run', libraries.HERE / 'run.py',
                     {'libraries': libraries, 'model': model, 'stage': stage, 'control': control})

# Copies private generated configuration entirely inside owned Agent A. Command
# output contains only fixed file names; keys and source tokens never reach the
# numbered public host command logs.
INSTALL = r'''
import os,stat,sys
root,gateway,executor,scene=sys.argv[1:]
def copy(source,target):
 fd=os.open(source,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and not s.st_mode&0o077
  raw=f.read((1<<20)+1);assert 0<len(raw)<=1<<20
 fd=os.open(target,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'wb') as f:f.write(raw);f.flush();os.fsync(f.fileno())
copy(root+'/gateway.json',gateway+'/config.json')
for source,target in [('executor.json','executor.json'),('planar.json','planar.json'),('executor-launch.json','launch.json')]:copy(root+'/'+source,executor+'/'+target)
for name in ['scene-read-token','scene-control-token']:copy(scene+'/'+name,executor+'/'+name)
print('mission service configuration installed')
'''

# Only this public trust document may enter numbered command output. The
# separately generated viewer profile contains the client key and stays private.
VIEWER_PUBLIC = r'''
import json,os,stat,sys
fd=os.open(sys.argv[1]+'/viewer-public.json',os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(fd,'rb') as f:
 s=os.fstat(f.fileno());assert stat.S_ISREG(s.st_mode) and s.st_uid==0 and s.st_nlink==1 and not s.st_mode&0o077
 raw=f.read((64<<10)+1);assert 0<len(raw)<=64<<10
 value=json.loads(raw);assert set(value)=={'version','tenant','site','audience','root_public_key','revocations'}
sys.stdout.buffer.write(raw)
'''


class Runner(base.Runner):
    def service_config(self, node, service, task):
        if service == 'viewer':
            model.require(node == 'b' and hasattr(self, 'viewer_public'), 'viewer public trust must be commissioned first')
            return model.viewer_config(self.pair, self.inputs['run_id'], self.scene, task, self.viewer_public)
        if service == 'people':
            model.require(node == 'a', 'people belongs to A')
            return model.people_config(self.pair, self.inputs['run_id'], self.scene,
                                       self.inputs['model'], task['generation'])
        if service == 'people-reader':
            model.require(node == 'b', 'people reader belongs to B')
            return model.people_reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
        model.require(service not in model.NON_WORLD_SERVICES, 'non-World service uses mission commissioning')
        return super().service_config(node, service, task)

    def deploy(self):
        self.step('apps')
        for key, node in self.nodes.items():
            node.check_world_startup()
            for service in model.SERVICES[key]:
                if service in model.NON_WORLD_SERVICES:
                    continue
                task = self.deploy_service(node, service)
                if service == 'viewer':
                    continue  # Public HTTP trust is emitted by mission commissioning.
                node.write(node.service_volume(service) + '/config.json',
                           model.json_bytes(self.service_config(key, service, task)))
        sockets = [value['socket_directory'] for key, value in self.tasks.items()
                   if key.split('/', 1)[1] not in model.NON_WORLD_SERVICES]
        model.require(len(sockets) == 12 and len(set(sockets)) == 12, 'twelve distinct ordinary World mounts required')
        self.commission_missions()
        self.install_viewer()
        for key, node in self.nodes.items():
            for service in model.SERVICES[key]:
                if service not in model.NON_WORLD_SERVICES:
                    node.write(node.service_volume(service) + '/go', b'')

    def commission_missions(self):
        self.step('mission-commission')
        a = self.nodes['a']
        task = self.deploy_service(a, 'mission-gateway')
        self.deploy_service(a, 'atlas-executor')
        presentation = None
        def rendered():
            nonlocal presentation
            response = a.python(control.HTTP, self.scene_url, '/v1/presentation',
                                a.service_volume('scene') + '/scene-read-token', '-', check=False, timeout=6)
            if response.returncode:
                return False
            presentation = model.strict_json(response.stdout, 4 << 20)
            return True
        a.wait(rendered, 15)
        office_relative = 'project/services/scene/source/examples/collaborative-office/scene.json'
        office = model.read_regular(str(self.source / 'a' / office_relative), 1 << 20)
        viewer = self.tasks['b/viewer']
        model.require(str(ipaddress.IPv4Address(viewer['ipv4'])) == viewer['ipv4'], 'observed viewer IPv4 required')
        viewer_url = getattr(self, 'viewer_url', '') or 'http://' + viewer['ipv4'] + ':' + str(model.VIEWER_PORT)
        cfg = model.mission_input(self.pair, self.inputs['run_id'], self.scene, presentation,
                                  self.scene_url, task['ipv4'], model.digest(office),
                                  getattr(self, 'operator_url', ''), viewer_url)
        path = stage.REMOTE + '/mission-input.json'
        a.write(path, model.json_bytes(cfg))
        self.mission_directory = a.service_volume('mission-gateway') + '/commissioned'
        a.inside(model.TOOLS + '/mission-commissioner', 'commission', '--input', path,
                 '--office-scene', stage.REMOTE + '/' + office_relative,
                 '--out', self.mission_directory)
        a.python(INSTALL, self.mission_directory, a.service_volume('mission-gateway'),
                 a.service_volume('atlas-executor'), a.service_volume('scene'))
        a.write(a.service_volume('mission-gateway') + '/go', b'')
        catalog = None
        def ready():
            nonlocal catalog
            response = a.inside(model.TOOLS + '/mission-commissioner', 'catalog',
                                '--credentials', self.mission_directory + '/executor.json', check=False, timeout=6)
            if response.returncode:
                return False
            catalog = model.strict_json(response.stdout, 64 << 10)
            model.require(catalog == {'assignee': 'atlas/navigation', 'domain': 'simulation',
                                     'run_id': self.inputs['run_id'],
                                     'types': [{'type': 'wendy.nav.inspect.v1', 'class': 'MOVE'}]},
                          'signed HTTP catalog differs from Atlas commissioning')
            return True
        a.wait(ready, 15)
        # The deferred source has not begun physics. The live owner opens this
        # gate only after its first successful actual source step.
        self.inspection_executor_started = False
        self.mission_commission = {'version': 1, 'input': cfg, 'catalog': catalog,
            'gateway_task': self.tasks['a/mission-gateway'], 'executor_task': self.tasks['a/atlas-executor'],
            'private_directory': self.mission_directory, 'mission_signed': False,
            'motion_requested': False, 'executor_gated_until_source_start': True,
            'operator_endpoint_configured': bool(cfg.get('operator_url'))}
        self.save('mission-commission.json', self.mission_commission)

    def install_viewer(self):
        raw = self.nodes['a'].python(VIEWER_PUBLIC, self.mission_directory).stdout
        self.viewer_public = model.strict_json(raw, 64 << 10)
        task = self.tasks['b/viewer']
        cfg = self.service_config('b', 'viewer', task)
        self.nodes['b'].write(self.nodes['b'].service_volume('viewer') + '/config.json', model.json_bytes(cfg))
        self.save('viewer-commission.json', {'version': 1, 'task': task,
            'public_sha256': model.digest(raw), 'config_sha256': model.digest(model.json_bytes(cfg)),
            'audience': model.VIEWER_AUDIENCE, 'listen': cfg['listen'], 'published': False,
            'native_evidence_enabled': bool(cfg['evidence_sources']),
            'media_available': False, 'read_mode': 'native_inference'})

    def start_inspection_executor(self):
        model.require(hasattr(self, 'mission_commission'), 'mission commissioning required')
        if self.inspection_executor_started:
            return
        a = self.nodes['a']
        a.write(a.service_volume('atlas-executor') + '/go', b'')
        self.inspection_executor_started = True
        self.save('mission-executor-start.json', {'version': 1, 'source_step_completed': True,
            'task': self.tasks['a/atlas-executor'], 'mission_signed': False, 'motion_requested': False})

    def observe(self):
        return {'mission_services_commissioned': True, 'mission_signed': False,
                'motion_requested': False, 'note': 'Live observation handoff and operator review are separate.'}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    parser.add_argument('--operator-url', default='', help='explicit observed client forwarding endpoint')
    args = parser.parse_args()
    runner = Runner(args.stage, args.output, trace_rpc=args.trace_rpc)
    runner.operator_url = args.operator_url
    runner.execute()
