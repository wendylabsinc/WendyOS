#!/usr/bin/env python3
"""New ordinary camera/people/remote-reader scenario; existing pair run is untouched."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tarfile
import threading
import time
from types import SimpleNamespace
import uuid

import libraries
import model
import stage
import control
library = libraries.runtime
startup = libraries.startup


LABEL = 'wendy.world.people'


MAX_PHASE_SECONDS = 600


TELEMETRY_STREAM_LIMIT = 1 << 20  # Per stdout/stderr stream, at most 2 MiB combined.


IPV6 = {'net.ipv6.conf.default.disable_ipv6': '0', 'net.ipv6.conf.default.accept_ra': '0',
        'net.ipv6.conf.default.autoconf': '0', 'net.ipv6.conf.default.router_solicitations': '0',
        'net.ipv6.conf.default.addr_gen_mode': '1', 'net.ipv6.conf.default.forwarding': '1',
        'net.ipv6.conf.all.forwarding': '1'}


WRITE = r'''
import hashlib,os,pathlib,sys
try:
 p=pathlib.Path(sys.argv[1]); assert p.is_absolute() and '..' not in p.parts
 raw=sys.stdin.buffer.read((96<<20)+1); assert len(raw)==int(sys.argv[2]) and hashlib.sha256(raw).hexdigest()==sys.argv[3]
 fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,int(sys.argv[4],8))
 with os.fdopen(fd,'wb') as f:f.write(raw);f.flush();os.fsync(f.fileno())
 print('file received')
except Exception:raise SystemExit(1)
'''


PAIR_SEND = r'''
import base64,json,os,pathlib,stat,sys
try:
 root=pathlib.Path('/var/lib/agentpair/enrollment'); node=sys.argv[1]; assert node in ('a','b')
 names=['pair-public.json',node+'/fixture-public.json',node+'/.vm-fixture/authority.json']; result={}
 for name in names:
  fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(fd,'rb') as f:
   st=os.fstat(f.fileno()); assert stat.S_ISREG(st.st_mode) and st.st_nlink==1 and st.st_uid==0 and not st.st_mode&0o077 and st.st_size<=65536
   raw=f.read(65537); assert len(raw)<=65536; result[name]=base64.b64encode(raw).decode()
 raw=json.dumps(result).encode(); assert len(raw)<=262144; sys.stdout.buffer.write(raw)
except Exception:raise SystemExit(1)
'''


PAIR_RECEIVE = r'''
import base64,hashlib,json,os,pathlib,sys
try:
 root=pathlib.Path('/var/lib/agentpair/enrollment'); node=sys.argv[1]; assert node in ('a','b')
 raw=sys.stdin.buffer.read(262145); assert len(raw)<=262144; values=json.loads(raw)
 names={'pair-public.json',node+'/fixture-public.json',node+'/.vm-fixture/authority.json'}; assert set(values)==names
 data={n:base64.b64decode(v,validate=True) for n,v in values.items()}; pair=json.loads(data['pair-public.json'])
 idx=0 if node=='a' else 1; assert pair['nodes'][idx]['directory']==str(root/node)
 assert hashlib.sha256(data[node+'/fixture-public.json']).hexdigest()==pair['nodes'][idx]['public_sha256']
 assert pair['pair_id']==sys.argv[2]; assert not root.exists()
 root.mkdir(mode=0o700); (root/node).mkdir(mode=0o700); (root/node/'.vm-fixture').mkdir(mode=0o700)
 for name in sorted(names):
  fd=os.open(root/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'wb') as f:f.write(data[name]);f.flush();os.fsync(f.fileno())
 print('own node staged')
except Exception:raise SystemExit(1)
'''


def runtime_args(image, name, network, volume, owner):
    args = library.runtime_args(image, name, network, volume)
    args[args.index(library.LABEL + '=' + name)] = LABEL + '=' + owner
    args[-1:-1] = ['--device', '/dev/net/tun:/dev/net/tun']
    for key, value in IPV6.items():
        args[-1:-1] = ['--sysctl', key + '=' + value]
    return args


def issuer_args(image, name, volume, owner):
    return ['docker', 'run', '--detach', '--name', name, '--label', LABEL + '=' + owner,
            '--network', 'none', '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
            '--memory', '256m', '--cpus', '1', '--pids-limit', '32', '--mount', 'type=volume,src=' + volume + ',dst=/var/lib',
            '--entrypoint', '/usr/bin/sleep', image, '900']


class Node:
    def __init__(self, owner, key):
        self.owner, self.key = owner, key
        self.name = owner.name + '-' + key
        self.volume = self.name + '-state'
        self.agent_pid = 0
        self.generation = 0
        self.command_sequence = 0
        self.task_state = None
        self.world_config_sha256 = None
        self.world_startup_checks = 0
        self.agent_phase = None
        self.runtime_identity = None
        self.command_counts = {}
        self.clock_raw = None
        self.mesh_raw = None
    def call(self, *args, **kwargs):
        return self.owner.call(*args, **kwargs)
    def target(self):
        if self.runtime_identity is None:
            return self.name
        return self.owner.current_container_id(self.name, self.runtime_identity)
    def inside(self, *args, timeout=60, check=True):
        return self.call(['docker', 'exec', self.target(), *args], timeout, check)
    python = library.Runner.python
    wait = library.Runner.wait
    snapshot_file = library.Runner.snapshot_file
    def cli(self, *args, timeout=300, check=True):
        environment = [item for key, value in library.base.ENV.items() for item in ('-e', key + '=' + value)]
        return self.call(['docker', 'exec', *environment, self.target(), model.TOOLS + '/cli', '--json', *args], timeout, check)
    def write(self, path, raw, mode=0o600):
        model.require(isinstance(raw, bytes) and len(raw) <= model.MAX_JSON, 'bounded public control write required')
        # Empty gates need EOF, not an interactive Docker stdin attachment.
        # Keep the same length/hash and exclusive receiver checks in both cases.
        interactive = ['-i'] if raw else []
        return self.call(['docker', 'exec', *interactive, self.target(), 'python3', '-c', WRITE, path, str(len(raw)), model.digest(raw), oct(mode)[2:]], input_data=raw if raw else None)
    def atomic_write(self, path, raw, *, previous=None):
        model.require(isinstance(raw, bytes) and 0 < len(raw) <= model.MAX_JSON and
                      (previous is None or isinstance(previous, bytes)), 'bounded atomic control required')
        result = self.call(['docker', 'exec', '-i', self.target(), 'python3', '-c', control.ATOMIC_WRITE,
                            path, str(len(raw)), model.digest(raw), '-' if previous is None else model.digest(previous)],
                           input_data=raw, timeout=10)
        model.require(model.strict_json(result.stdout, 4096) == {'bytes': len(raw), 'sha256': model.digest(raw), 'atomic': True},
                      'atomic control receipt differs; outcome not retried')
    def read(self, path, limit=model.MAX_JSON):
        model.require(0 < limit <= 1 << 20, 'bounded read required')
        script = "import os,stat,sys; f=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW); s=os.fstat(f); assert stat.S_ISREG(s.st_mode) and s.st_nlink==1 and s.st_size<=int(sys.argv[2]); b=os.read(f,int(sys.argv[2])+1); assert len(b)==s.st_size; sys.stdout.buffer.write(b)"
        return self.python(script, path, limit).stdout
    def start_agent(self, phase):
        model.require(phase in ('enrollment', 'commissioned', 'restart'), 'invalid agent phase')
        env = ['-e', 'WENDY_CONFIG_PATH=' + model.config_path(self.key), '-e', 'WENDY_CONTAINERD_ADDR=/run/containerd/containerd.sock']
        if self.owner.trace_rpc:
            env += ['-e', 'WENDY_WORLD_RPC_TIMING=1']
        self.inside('mkdir', '-p', '/var/lib/agentvm/logs')
        self.call(['docker', 'exec', '-d', *env, self.target(), 'sh', '-c',
                   'umask 077; echo $$ > /run/agentvm/agent.pid; exec /var/lib/agentpair/tools/agent > "/var/lib/agentvm/logs/agent-$1.log" 2>&1', 'sh', phase])
        def ready():
            result = self.python("import os,sys; p=int(open('/run/agentvm/agent.pid').read()); assert p>1 and p!=int(sys.argv[1]); assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentpair/tools/agent'; print(p)", self.agent_pid, check=False)
            if result.returncode or self.cli('device', 'apps', 'list', timeout=8, check=False).returncode:
                return False
            pid = int(result.stdout)
            self.python("import os,sys; assert os.readlink('/proc/'+sys.argv[1]+'/exe')=='/var/lib/agentpair/tools/agent'", pid)
            self.agent_pid = pid
            return True
        self.wait(ready, 75)
        self.agent_phase = phase
        if phase != 'enrollment':
            self.check_world_startup()
    def check_world_startup(self):
        model.require(self.agent_phase in ('commissioned', 'restart') and self.agent_pid > 1 and
                      model.digest_shape(self.world_config_sha256), 'pinned commissioned process required')
        identity = self.owner.current_container_id(self.name, self.runtime_identity)
        response = self.call(['docker', 'exec', identity, 'python3', '-c', startup.PROBE_SCRIPT,
                              model.config_path(self.key) + '/world.json', self.world_config_sha256,
                              '/var/lib/agentvm/logs/agent-' + self.agent_phase + '.log', str(self.agent_pid)],
                             timeout=10, max_output=4096)
        report = model.strict_json(response.stdout, 4096)
        keys = {'known_startup_failure', 'reasons', 'world_ready', 'scope_behavior_check_pending',
                'inspection_complete', 'config_hash_matches'}
        reasons = {'publication_unavailable', 'publication_fresh_quorum_missing', 'replication_unavailable',
                   'startup_log_limit', 'recognized_startup_log_invalid', 'startup_inspection_refused'}
        model.require(isinstance(report, dict) and set(report) == keys and
                      all(type(report[key]) is bool for key in keys - {'reasons'}) and
                      isinstance(report['reasons'], list) and len(report['reasons']) <= 3 and
                      all(isinstance(reason, str) and reason in reasons for reason in report['reasons']) and
                      report['world_ready'] is False and report['scope_behavior_check_pending'] is True,
                      'invalid sanitized startup result')
        self.world_startup_checks += 1
        self.owner.save(f'world-startup-{self.key}-{self.world_startup_checks}.json', {
            'version': 1, 'node': self.key, 'phase': self.agent_phase, 'agent_pid': self.agent_pid,
            'container_id': identity, 'expected_config_sha256': self.world_config_sha256, **report})
        model.require(report['inspection_complete'] and report['config_hash_matches'] and
                      not report['known_startup_failure'] and not report['reasons'],
                      'commissioned World startup refused or inspection incomplete')
        return report
    def stop_agent(self):
        model.require(self.agent_pid > 1, 'known owned agent required')
        self.python("import os,signal,sys; p=int(sys.argv[1]); assert os.readlink('/proc/'+str(p)+'/exe')=='/var/lib/agentpair/tools/agent'; os.kill(p,signal.SIGTERM)", self.agent_pid)
        self.wait(lambda: self.python("import os,sys; assert not os.path.exists('/proc/'+sys.argv[1])", self.agent_pid, check=False).returncode == 0, 50)

    def task(self, service):
        model.require(service in model.SERVICES[self.key], 'commissioned service required')
        cid = model.APP + '_' + service
        info = model.strict_json(self.inside('ctr', 'containers', 'info', cid).stdout)
        labels = info.get('Labels', {})
        model.require(labels.get('sh.wendy/app.id') == model.APP and labels.get('sh.wendy/service') == service, 'ordinary task labels differ')
        rows = [line.split() for line in self.inside('ctr', 'tasks', 'list').stdout.decode().splitlines()[1:]]
        rows = [row for row in rows if row and row[0] == cid]
        model.require(len(rows) == 1 and rows[0][2] == 'RUNNING', 'running ordinary task required')
        pid = int(rows[0][1])
        process = model.strict_json(self.python("import json,os,sys; p=int(sys.argv[1]); print(json.dumps({'pid':p,'start_ticks':int(open('/proc/'+str(p)+'/stat').read().rsplit(')',1)[1].split()[19]),'netns_inode':os.stat('/proc/'+str(p)+'/ns/net').st_ino,'cgroup':open('/proc/'+str(p)+'/cgroup').read().strip()}))", pid).stdout)
        addresses = model.strict_json(self.inside('nsenter', '--target', str(pid), '--net', 'ip', '-j', '-4', 'addr', 'show', 'dev', 'eth0').stdout)
        ips = [a['local'] for row in addresses for a in row.get('addr_info', []) if a.get('family') == 'inet']
        model.require(len(ips) == 1, 'one CNI endpoint required')
        socket = labels.get('sh.wendy/world.socket-directory', '')
        generation = int(labels.get('sh.wendy/world.deployment-generation', '0'))
        non_world = service in getattr(model, 'NON_WORLD_SERVICES', ('scene',))
        model.require((not socket and not generation) if non_world else (socket.startswith(model.state_path(self.key) + '/sockets/') and 0 < generation < 1 << 64), 'World entitlement differs')
        return {**process, 'node': self.key, 'container': cid, 'app': model.APP, 'service': service, 'device': self.owner.pair['nodes'][('a', 'b').index(self.key)]['public']['principal'], 'generation': generation, 'socket_directory': socket, 'ipv4': ips[0]}

    def service_volume(self, service, kind='config'):
        return '/var/lib/wendy/volumes/' + model.volume_names(self.owner.inputs['run_id'], self.key, service)[kind]

    def latest(self, service):
        result = self.python(control.LATEST, self.service_volume(service, 'proofs') + '/stdout.json', 2 << 20, check=False)
        return None if result.returncode else model.strict_json(result.stdout, 2 << 20)

    def scene_request(self, path, value=None):
        filename = 'scene-control-token' if value is not None else 'scene-read-token'
        body = '-' if value is None else model.json_bytes(value).decode()
        result = self.python(control.HTTP, self.owner.scene_url, path, self.service_volume('scene') + '/' + filename, body, timeout=6)
        return result.stdout


class Runner:
    call = library.Runner.call

    def __init__(self, prepared, output, *, trace_rpc=False):
        self.source = Path(prepared).resolve()
        self.inputs = stage.validate(self.source)
        self.out = libraries.stage.safe_output(Path(output).resolve(), stage.ROOT / '.local')
        self.name = 'world-people-' + uuid.uuid4().hex[:12]
        self.network, self.issuer = self.name + '-net', self.name + '-issuer'
        self.issuer_volume = self.issuer + '-state'
        self.nodes = {key: Node(self, key) for key in ('a', 'b')}
        self.trace_rpc, self.created, self.count = trace_rpc, [], 0
        self.pair = self.peers = self.scene = self.scene_url = None
        self.tasks = {}
        self.started, self.runtime_state = time.monotonic(), 'new'

    def upload_stage(self, node):
        source = self.source / node.key
        raw, files, directories = library.upload_inventory(source)
        node.python(library.CREATE_STAGE, stage.REMOTE)
        self.count += 1
        stem = self.out / f'{self.count:03d}'
        command = ['docker', 'exec', '-i', node.target(), 'tar', '--no-same-owner', '-xf', '-', '-C', stage.REMOTE]
        with stem.with_suffix('.stdout').open('xb') as stdout, stem.with_suffix('.stderr').open('xb') as stderr:
            process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=stdout, stderr=stderr, start_new_session=True)
            timer = threading.Timer(180, lambda: os.killpg(process.pid, signal.SIGKILL) if process.poll() is None else None)
            timer.start()
            try:
                with tarfile.open(fileobj=process.stdin, mode='w|') as archive:
                    library.write_stage_archive(archive, source, raw, files, directories)
                process.stdin.close()
                code = process.wait(timeout=10)
            finally:
                timer.cancel()
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=5)
        self.save(stem.name + '.json', {'argv': command, 'exit_code': code})
        model.require(code == 0, 'stage transfer refused')
        node.python(library.VERIFY_STAGE, stage.REMOTE, model.digest(raw))
        for name in stage.TOOLS:
            node.inside('cp', stage.REMOTE + '/tools/' + name, model.TOOLS + '/' + name)

    def setup(self):
        self.step('setup')
        self.save('inputs.json', self.inputs)
        self.call(['docker', 'image', 'inspect', self.inputs['runtime_image']])
        self.create('network', self.network, ['docker', 'network', 'create', '--label', LABEL + '=' + self.name, self.network])
        for name in (self.issuer_volume, *[n.volume for n in self.nodes.values()]):
            self.create('volume', name, ['docker', 'volume', 'create', '--label', LABEL + '=' + self.name, name])
        self.create('container', self.issuer, issuer_args(self.inputs['runtime_image'], self.issuer, self.issuer_volume, self.name))
        self.issuer_identity = self.current_container_id(self.issuer)
        for path in ('/var/lib/agentpair', model.TOOLS):
            self.call(['docker', 'exec', self.issuer_identity, 'python3', '-c', library.CREATE_STAGE, path])
        self.upload_binary(self.issuer_identity, 'pair-fixture')
        for node in self.nodes.values():
            self.create('container', node.name, runtime_args(self.inputs['runtime_image'], node.name, self.network, node.volume, self.name))
            node.runtime_identity = self.current_container_id(node.name)
            node.wait(lambda: node.python("import json; assert json.load(open('/run/agentvm/ready.json'))['ready'] is True", check=False).returncode == 0, 75)
            node.python("import fcntl,os,struct; f=os.open('/dev/net/tun',os.O_RDWR); assert len(fcntl.ioctl(f,0x800454cf,struct.pack('I',0)))==4; os.close(f)")
            for path in ('/var/lib/agentpair', model.TOOLS, model.PROJECT, model.state_path(node.key)):
                node.python(library.CREATE_STAGE, path)
            self.upload_stage(node)
    def save(self, name, value):
        library.stage.write_new(self.out / name, model.json_bytes(value))
    def step(self, value):
        model.require(time.monotonic() - self.started <= MAX_PHASE_SECONDS, 'total runtime gate budget expired')
        self.runtime_state = value
        self.save('phase-' + value + '.json', {'phase': value})
    def create(self, kind, name, argv):
        self.created.append((kind, name))  # Include an ambiguous create in cleanup.
        return self.call(argv)
    def upload_binary(self, container, name):
        row = self.inputs['binaries'][name]
        raw = model.read_regular(row['path'], 96 << 20)
        model.require(model.digest(raw) == row['sha256'], 'binary changed before upload')
        # Binary stdin is not logged. The fixed receiver emits a constant marker.
        self.count += 1; stem = self.out / f'{self.count:03d}'
        command = ['docker', 'exec', '-i', container, 'python3', '-c', WRITE, model.TOOLS + '/' + name, str(len(raw)), row['sha256'], '755']
        with open(row['path'], 'rb') as source, stem.with_suffix('.stdout').open('xb') as out, stem.with_suffix('.stderr').open('xb') as err:
            try:
                result = subprocess.run(command, stdin=source, stdout=out, stderr=err, timeout=120)
                code = result.returncode
            except subprocess.TimeoutExpired:
                code = 124
        self.save(stem.name + '.json', {'argv': command, 'exit_code': code, 'input_sha256': row['sha256'], 'input_bytes': len(raw)})
        model.require(code == 0 and stem.with_suffix('.stdout').read_bytes() == b'file received\n', 'binary upload refused')
    def stage_node_secrets(self, node):
        # No host log receives the source stdout; it is directly the receiver's stdin.
        source_cmd = ['docker', 'exec', self.current_container_id(self.issuer, self.issuer_identity), 'python3', '-c', PAIR_SEND, node.key]
        target_cmd = ['docker', 'exec', '-i', node.target(), 'python3', '-c', PAIR_RECEIVE, node.key, self.pair['pair_id']]
        source = target = None
        try:
            source = subprocess.Popen(source_cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            target = subprocess.Popen(target_cmd, stdin=source.stdout, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            source.stdout.close()
            output, _ = target.communicate(timeout=30)
            source_code = source.wait(timeout=5)
            target_code = target.returncode
            model.require(source_code == 0 and target_code == 0 and output == b'own node staged\n', 'isolated node staging refused')
        finally:
            for process in (source, target):
                if process is not None and process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=5)
        self.save('issuer-transfer-' + node.key + '.json', {'own_node_only': True, 'host_payload_retained': False, 'pair_id': self.pair['pair_id'], 'source_exit': source_code, 'receiver_exit': target_code})
    def enroll(self):
        self.step('enrollment')
        command = [model.TOOLS + '/pair-fixture', 'init', '--directory', model.PAIR,
                   '--state-directory-a', model.state_path('a'), '--state-directory-b', model.state_path('b')]
        result = self.call(['docker', 'exec', self.current_container_id(self.issuer, self.issuer_identity), *command])
        self.pair = model.validate_pair(model.strict_json(result.stdout))
        self.save('pair-public.json', self.pair)
        for node in self.nodes.values():
            self.stage_node_secrets(node)
            node.write(model.config_path(node.key) + '/network-access', b'network\n')
            node.inside('mkdir', '-p', '/var/lib/agentvm/logs')
            node.call(['docker', 'exec', '-d', node.target(), 'sh', '-c',
                       'exec /var/lib/agentpair/tools/fixture serve --directory "$1" --listen 127.0.0.1:0 --handoff /config/acme-enrollment.json --duration 10m > /var/lib/agentvm/logs/issuer.log 2>&1', 'sh', model.config_path(node.key)])
            node.wait(lambda: node.python("import os; assert os.path.isfile('/config/acme-enrollment.json')", check=False).returncode == 0, 30)
            node.start_agent('enrollment')
            node.wait(lambda: node.python("import json,os,sys; p=sys.argv[1]; assert json.load(open(p+'/provisioning.json')).get('enrolled'); assert os.path.isfile(p+'/.vm-fixture/issued.json')", model.config_path(node.key), check=False).returncode == 0, 90)
            issued = node.read(model.config_path(node.key) + '/.vm-fixture/issued.json', 64 << 10)
            model.strict_json(issued)
            library.stage.write_new(self.out / ('issued-' + node.key + '.json'), issued)
            self.call(['docker', 'exec', '-i', self.current_container_id(self.issuer, self.issuer_identity), 'python3', '-c', WRITE, model.config_path(node.key) + '/.vm-fixture/issued.json', str(len(issued)), model.digest(issued), '600'], input_data=issued)
        public = self.call(['docker', 'exec', self.current_container_id(self.issuer, self.issuer_identity), model.TOOLS + '/pair-fixture', 'peers', '--directory', model.PAIR])
        self.peers = model.validate_peers(self.pair, model.strict_json(public.stdout))
        self.save('pair-peers.json', self.peers)
    def projects(self):
        self.step('projects')
        for key, node in self.nodes.items():
            node.write(stage.REMOTE + '/project/wendy.json', model.json_bytes(model.app_manifest(self.pair, self.inputs['run_id'], key)), 0o644)
            for service in model.SERVICES[key]:
                names = list(model.volume_names(self.inputs['run_id'], key, service).values())
                raw = node.python(control.VOLUME_DIRECTORIES, *names).stdout
                self.save('volumes-' + key + '-' + service + '.json', control.volume_receipt(raw, names))
        a = self.nodes['a']
        paths = [a.service_volume(s) for s in ('scene', 'camera-a', 'camera-b')]
        a.python("import os,secrets,sys\npaths=sys.argv[1:]; values=[secrets.token_urlsafe(32),secrets.token_urlsafe(32)]\n"
                 "def put(p,v):\n f=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)\n with os.fdopen(f,'w') as s:s.write(v+'\\n');s.flush();os.fsync(s.fileno())\n"
                 "for p in paths:put(p+'/scene-read-token',values[0])\nput(paths[0]+'/scene-control-token',values[1])\nprint('source credentials created in owned node')", *paths)

    def deploy_service(self, node, service):
        node.cli('run', '--prefix', stage.REMOTE + '/project', '--builder', 'buildkit', '--chunking', 'force', '--detach', '--no-restart', '--skip-cloud-registration', '--yes', '--service', service)
        task = node.task(service)
        self.tasks[node.key + '/' + service] = task
        self.save('task-' + node.key + '-' + service + '.json', task)
        return task

    def source_scene(self):
        self.step('scene')
        a = self.nodes['a']
        task = self.deploy_service(a, 'scene')
        self.scene_url = 'http://' + task['ipv4'] + ':8091'
        def ready():
            result = a.python(control.HTTP, self.scene_url, '/v1/identity', a.service_volume('scene') + '/scene-read-token', '-', check=False, timeout=6)
            if result.returncode:
                return False
            self.scene = model.validate_scene_identity(self.inputs['run_id'], model.strict_json(result.stdout))
            return True
        a.wait(ready, 30)
        self.save('scene-identity.json', self.scene)

    def commission(self):
        self.step('commission')
        addresses = {}
        for key, node in self.nodes.items():
            rows = model.strict_json(self.call(['docker', 'inspect', node.target()]).stdout, 1 << 20)
            addresses[key] = rows[0]['NetworkSettings']['Networks'][self.network]['IPAddress']
        self.save('observed-addresses.json', addresses)
        for key, node in self.nodes.items():
            node.stop_agent()
            kwargs = {'scene_url': self.scene_url, 'read_token_file': node.service_volume('scene') + '/scene-read-token'} if key == 'a' else {}
            cfg = model.commission(key, self.pair, self.peers, self.inputs['run_id'], self.scene, **kwargs)
            raw = model.json_bytes(cfg)
            node.write(model.config_path(key) + '/world.json', raw)
            node.world_config_sha256 = model.digest(raw)
            if key == 'b':
                node.clock_raw = control.sample_clock(self.inputs['run_id'], self.inputs['anchor_unix_ns'], 0)
                node.write(model.config_path(key) + '/simclock.json', node.clock_raw)
            node.mesh_raw = model.json_bytes(model.mesh_configuration(self.pair, key, addresses))
            node.write(model.config_path(key) + '/local-mesh.json', node.mesh_raw)
            node.write(model.config_path(key) + '/nan-mesh.json', model.json_bytes({'participate': True, 'roam': False, 'shareUplink': False}))
            self.save('commission-' + key + '.json', cfg)
            node.inside(model.TOOLS + '/initializer', '--config', model.config_path(key) + '/world.json', timeout=45)
        for node in self.nodes.values():
            node.start_agent('commissioned')
        after = self.nodes['a'].task('scene')
        model.require(after == self.tasks['a/scene'], 'scene task changed across agent restart')
        model.require(model.strict_json(self.nodes['a'].scene_request('/v1/identity')) == self.scene, 'scene identity changed')
        self.save('scene-continuity.json', {'same_task': True, 'same_identity': True, 'task': after})

    def deploy(self):
        self.step('apps')
        for key, node in self.nodes.items():
            node.check_world_startup()
            for service in model.SERVICES[key]:
                if service == 'scene':
                    continue
                task = self.deploy_service(node, service)
                if service in model.CAMERAS:
                    cfg = model.camera_config(service, self.inputs['run_id'], self.scene, self.scene_url)
                elif service == 'people':
                    cfg = model.people_config(self.pair, self.inputs['run_id'], self.scene, self.inputs['model'], task['generation'])
                else:
                    cfg = model.reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                node.write(node.service_volume(service) + '/config.json', model.json_bytes(cfg))
        sockets = [v['socket_directory'] for k, v in self.tasks.items() if k != 'a/scene']
        model.require(len(sockets) == 4 and len(set(sockets)) == 4, 'distinct ordinary World mounts required')
        for key, node in self.nodes.items():
            for service in model.SERVICES[key]:
                if service != 'scene':
                    node.write(node.service_volume(service) + '/go', b'')

    def snapshot_outputs(self, node, identity):
        saved = {}
        if self.trace_rpc:
            saved['telemetry'] = self.snapshot_telemetry(node, identity)
        for phase in ('enrollment', 'commissioned'):
            self.current_container_id(node.name, identity)
            name = 'agent-' + phase + '.log'
            saved[name] = node.snapshot_file('/var/lib/agentvm/logs/' + name, self.out / 'private-runtime-logs' / node.key / name, 8 << 20, container=identity)
        for service in model.SERVICES[node.key]:
            for name, limit in (('exit.json', 4096), ('stdout.json', 32 << 20), ('stderr.log', 1 << 20)):
                self.current_container_id(node.name, identity)
                saved[service + '/' + name] = node.snapshot_file(node.service_volume(service, 'proofs') + '/' + name, self.out / 'apps' / node.key / service / name, limit, container=identity)
        self.save('snapshots-' + node.key + '.json', saved)

    def execute(self):
        import scenario
        result = {'passed': False, 'simulation_only': True, 'remote_camera_verification': False}
        prior = {}
        def interrupted(signum, _):
            raise library.InterruptedRun(signum)
        for signum in (signal.SIGTERM, signal.SIGINT):
            prior[signum] = signal.signal(signum, interrupted)
        try:
            try:
                self.setup(); self.enroll(); self.projects(); self.source_scene(); self.commission(); self.deploy()
                result['observations'] = scenario.execute(self)
                result['gate_completed'] = True
            except BaseException as error:
                result.update(gate_completed=False, error=type(error).__name__, failed_phase=self.runtime_state)
            finally:
                cleanup = self.cleanup()
                result['cleanup'] = cleanup
                result['passed'] = result.get('gate_completed', False) and cleanup['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'people scenario or owned cleanup failed; receipts retained')
            return result
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)

    def current_container_id(self, name, expected=None):
        result = self.call(['docker', 'inspect', name], check=False)
        model.require(result.returncode == 0, 'owned container unavailable')
        items = model.strict_json(result.stdout, 1 << 20)
        model.require(isinstance(items, list) and len(items) == 1, 'exact container required')
        info = items[0]
        identity = info.get('Id', '')
        model.require(isinstance(identity, str) and re.fullmatch('[a-f0-9]{64}', identity) and
                      (info.get('Config', {}).get('Labels', {}) or {}).get(LABEL) == self.name and
                      (expected is None or identity == expected), 'container ownership or incarnation changed')
        return identity
    def snapshot_telemetry(self, node, identity):
        # The ordinary production logger already tees Debug events into the
        # agent's telemetry buffer. Keep its JSON stderr logger unchanged.
        # This collector must not use self.call: debug attributes can contain
        # private errors and device identifiers, including on a failed command.
        report = {'status': 'unknown', 'event_completeness': 'unknown', 'files': {}}
        directory = None
        try:
            model.require(node.key in ('a', 'b'), 'known telemetry node required')
            private = self.out / 'private-runtime-telemetry'
            private.mkdir(mode=0o700, exist_ok=True)
            info = private.lstat()
            model.require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and
                          not info.st_mode & 0o077, 'private telemetry directory required')
            target = private / node.key
            target.mkdir(mode=0o700, exist_ok=False)
            directory = target
            environment = [item for key, value in library.base.ENV.items() for item in ('-e', key + '=' + value)]
            # Recheck ownership immediately before exec and target the exact ID,
            # never the reusable container name. No publication or repair call.
            self.current_container_id(node.name, identity)
            response = library.Runner.call(SimpleNamespace(out=directory, count=0),
                ['docker', 'exec', *environment, identity, model.TOOLS + '/cli', '--json',
                 'device', 'logs', '--service', 'wendy-agent', '--level', 'debug',
                 '--tail', '512', '--no-follow', '--read-only'], timeout=15, check=False,
                max_output=TELEMETRY_STREAM_LIMIT)
            if response.returncode == 0:
                report['status'] = 'captured'
        except (Exception, library.InterruptedRun):
            # Diagnostics cannot prevent resource cleanup. Missing events,
            # partial replay, refusal, timeout and an interrupted collector
            # remain unknown. The collector has already reaped its child.
            pass
        if directory is not None:
            for name, limit in (('001.json', 8192), ('001.stdout', TELEMETRY_STREAM_LIMIT),
                                ('001.stderr', TELEMETRY_STREAM_LIMIT)):
                try:
                    fd = os.open(directory / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    with os.fdopen(fd, 'rb') as stream:
                        info = os.fstat(stream.fileno())
                        model.require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and
                                      info.st_uid == os.geteuid() and 0 <= info.st_size <= limit,
                                      'bounded private telemetry file required')
                        raw = stream.read(limit + 1)
                        model.require(len(raw) == info.st_size, 'telemetry file changed')
                    report['files'][name] = {'size': len(raw), 'sha256': model.digest(raw)}
                except Exception:
                    report['status'] = 'unknown'
        return report
    def resource_state(self, kind, name):
        args = ['docker', 'inspect', name] if kind == 'container' else ['docker', kind, 'inspect', name]
        result = self.call(args, check=False)
        if result.returncode:
            # Exact-name listing distinguishes absent from an inspect failure.
            listing = self.call(['docker', kind, 'ls', '--format', '{{.Name}}'] if kind != 'container' else ['docker', 'ps', '-a', '--format', '{{.Names}}'], check=False)
            model.require(listing.returncode == 0, 'resource presence unknown')
            return 'absent' if name.encode() not in listing.stdout.splitlines() else 'unknown'
        info = model.strict_json(result.stdout, 1 << 20)[0]
        labels = info.get('Config', {}).get('Labels', {}) if kind == 'container' else info.get('Labels', {})
        return 'owned' if (labels or {}).get(LABEL) == self.name else 'foreign'
    def cleanup(self):
        rows = []
        for kind, name in reversed(self.created):
            row = {'kind': kind, 'name': name}
            try:
                state = self.resource_state(kind, name)
                row['before'] = state
                if state == 'owned':
                    if kind == 'container':
                        identity = self.current_container_id(name)
                        row['container_id'] = identity
                        node = next((n for n in self.nodes.values() if n.name == name), None)
                        if node is not None:
                            try:
                                self.snapshot_outputs(node, identity)
                            except Exception as error:
                                row['snapshot_error'] = type(error).__name__
                        self.current_container_id(name, identity)
                        args = ['docker', 'rm', '-f', identity]
                    else:
                        model.require(self.resource_state(kind, name) == 'owned', 'resource owner changed')
                        args = ['docker', kind, 'rm', name]
                    self.call(args, check=False)
                row['after'] = self.resource_state(kind, name)
            except Exception as error:
                row.update(after='unknown', error=type(error).__name__)
            rows.append(row)
        result = {'resources': rows, 'complete': all(row['after'] == 'absent' for row in rows)}
        self.save('cleanup.json', result)
        return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, trace_rpc=args.trace_rpc).execute()


if __name__ == '__main__':
    main()
