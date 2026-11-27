#!/usr/bin/env python3
"""Explicit two-agent disposable fixture gates. No runtime starts on import."""
import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import time
from types import SimpleNamespace
import uuid

import model
import startup
import control

HERE = Path(__file__).resolve().parent
# Reuse the bounded process collector and protected runtime setup. The existing
# seven-service runner is never constructed or executed by this module.
sys.path.insert(0, str(HERE.parent / 'agentmultimodal'))
try:
    spec = importlib.util.spec_from_file_location('pair_multimodal_library', HERE.parent / 'agentmultimodal/run.py')
    library = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(library)
finally:
    sys.path.pop(0)

LABEL = 'wendy.world.agentpair'
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

# Exactly one node's issuer material travels pipe-to-pipe. It never reaches a
# host receipt, buffer or file. The consumer accepts only these fixed members.
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
        return self.call(['docker', 'exec', '-i', self.target(), 'python3', '-c', WRITE, path, str(len(raw)), model.digest(raw), oct(mode)[2:]], input_data=raw)

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

    def observe_task(self):
        cid = model.APP + '_' + model.SERVICE
        info = model.strict_json(self.inside('ctr', 'containers', 'info', cid).stdout)
        labels = info.get('Labels', {})
        model.require(labels.get('sh.wendy/app.id') == model.APP and labels.get('sh.wendy/service') == model.SERVICE, 'unexpected ordinary task')
        generation = int(labels.get('sh.wendy/world.deployment-generation', '0'))
        socket = labels.get('sh.wendy/world.socket-directory', '')
        model.require(0 < generation < 1 << 64 and socket.startswith(model.state_path(self.key) + '/sockets/'), 'scoped World mount required')
        rows = [line.split() for line in self.inside('ctr', 'tasks', 'list').stdout.decode().splitlines()[1:]]
        rows = [row for row in rows if row and row[0] == cid]
        model.require(len(rows) == 1 and len(rows[0]) >= 3 and rows[0][2] == 'RUNNING' and int(rows[0][1]) > 1, 'running ordinary app required')
        pid = int(rows[0][1])
        process = model.strict_json(self.python("import json,os,sys; p=int(sys.argv[1]); print(json.dumps({'pid':p,'cgroup':open('/proc/'+str(p)+'/cgroup').read().strip(),'start_ticks':int(open('/proc/'+str(p)+'/stat').read().rsplit(')',1)[1].split()[19])}))", pid).stdout)
        self.task_state = {**process, 'app': model.APP, 'service': model.SERVICE, 'device': self.owner.pair['nodes'][('a', 'b').index(self.key)]['public']['principal'], 'generation': generation, 'socket_directory': socket, 'container': cid}
        return self.task_state

    def command(self, action, *, expect_refusal=False, **fields):
        model.require(self.task_state is not None and self.command_sequence < 32, 'running bounded probe required')
        model.require(not expect_refusal or action == 'query', 'only the explicit old-mount read may expect refusal')
        raw = control.command(action, self.command_sequence + 1, fields)
        self.command_sequence += 1  # An ambiguous write is never reused/retried.
        self.command_counts[self.generation] = self.command_sequence
        name = f'{self.command_sequence:06d}.json'
        names = model.volume_names(self.owner.inputs['run_id'], self.key, self.generation)
        self.atomic_write('/var/lib/wendy/volumes/' + names['control'] + '/' + name, raw)
        path = '/var/lib/wendy/volumes/' + names['proofs'] + '/' + name
        self.wait(lambda: self.python("import os,sys; assert os.path.isfile(sys.argv[1])", path, check=False).returncode == 0, 15)
        exact = self.read(path, 512 << 10)
        # Save before decoding or checking success, including unknown outcomes.
        folder = self.owner.out / 'probe' / self.key
        if self.generation != 1:
            folder /= 'generation-' + str(self.generation)
        library.stage.write_new(folder / name, exact)
        receipt = model.strict_json(exact, 512 << 10)
        model.require(receipt.get('version') == 1 and receipt.get('run_id') == self.owner.inputs['run_id'] and receipt.get('sequence') == self.command_sequence and receipt.get('action') == action, 'command receipt binding differs')
        model.require(receipt.get('command_sha256') == model.digest(raw) and base64.b64decode(receipt.get('command_json', ''), validate=True) == raw, 'original command bytes differ')
        model.require(receipt.get('validation') == 'socket_scope_and_structure_only', 'probe validation marker differs')
        if expect_refusal:
            model.require(receipt.get('status') == 'failed' and receipt.get('originals') == [] and
                          not receipt.get('reply_json') and receipt.get('error') in ('socket_or_clock_refused', 'deadline_exceeded'),
                          'old-mount read was not explicitly refused without data')
            return receipt
        model.require(receipt.get('status') == 'complete', 'probe refused or outcome unknown')
        if action != 'finish':
            clock = receipt.get('clock', {})
            model.require(clock.get('deployment_generation') == self.task_state['generation'] and clock.get('sample_clock_domain') == model.sample_domain(self.owner.inputs['run_id']), 'current probe reader generation differs')
        return receipt


class Runner:
    call = library.Runner.call

    def __init__(self, inputs, output, *, phase='enrollment', trace_rpc=False):
        # All binaries and input structure are checked before any output/runtime mutation.
        self.inputs = model.validate_inputs(model.read_regular(inputs, model.MAX_JSON))
        model.require(phase in ('enrollment', 'commission', 'deploy', 'replication'), 'explicit known gate required')
        model.require(phase != 'replication' or 'proof' in self.inputs['binaries'], 'replication requires independently pinned proof utility')
        self.out = Path(output).resolve()
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.name = 'world-agentpair-' + uuid.uuid4().hex[:12]
        self.network = self.name + '-net'
        self.issuer = self.name + '-issuer'
        self.issuer_volume = self.issuer + '-state'
        self.nodes = {name: Node(self, name) for name in ('a', 'b')}
        self.phase, self.trace_rpc = phase, trace_rpc
        self.created = []
        self.count = 0
        self.pair = self.peers = None
        self.started = time.monotonic()
        self.runtime_state = 'new'

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
        source_cmd = ['docker', 'exec', self.issuer, 'python3', '-c', PAIR_SEND, node.key]
        target_cmd = ['docker', 'exec', '-i', node.name, 'python3', '-c', PAIR_RECEIVE, node.key, self.pair['pair_id']]
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

    def setup(self):
        self.step('setup')
        self.save('inputs.json', self.inputs)
        self.call(['docker', 'image', 'inspect', self.inputs['runtime_image']])
        self.create('network', self.network, ['docker', 'network', 'create', '--label', LABEL + '=' + self.name, self.network])
        for name in [self.issuer_volume, *[n.volume for n in self.nodes.values()]]:
            self.create('volume', name, ['docker', 'volume', 'create', '--label', LABEL + '=' + self.name, name])
        self.create('container', self.issuer, issuer_args(self.inputs['runtime_image'], self.issuer, self.issuer_volume, self.name))
        # Docker exec may use umask 0000; mkdir -p -m only protects its final
        # component. Create each fresh private root through anchored nofollow
        # checks before writing any issuer material or tools below it.
        for path in ('/var/lib/agentpair', model.TOOLS):
            self.call(['docker', 'exec', self.issuer, 'python3', '-c', library.CREATE_STAGE, path])
        self.upload_binary(self.issuer, 'pair-fixture')
        for node in self.nodes.values():
            self.create('container', node.name, runtime_args(self.inputs['runtime_image'], node.name, self.network, node.volume, self.name))
            node.wait(lambda: node.python("import json; assert json.load(open('/run/agentvm/ready.json'))['ready'] is True", check=False).returncode == 0, 75)
            node.python("import fcntl,os,struct; f=os.open('/dev/net/tun',os.O_RDWR); flags=fcntl.ioctl(f,0x800454cf,struct.pack('I',0)); assert len(flags)==4; os.close(f); print('TUN available')")
            for path in ('/var/lib/agentpair', model.TOOLS, model.PROJECT, model.state_path(node.key)):
                node.python(library.CREATE_STAGE, path)
            for name in model.TOOLS_REQUIRED:
                if name != 'pair-fixture':
                    self.upload_binary(node.name, name)
            if self.phase == 'replication':
                self.upload_binary(node.name, 'proof')

    def enroll(self):
        self.step('enrollment')
        command = [model.TOOLS + '/pair-fixture', 'init', '--directory', model.PAIR,
                   '--state-directory-a', model.state_path('a'), '--state-directory-b', model.state_path('b')]
        result = self.call(['docker', 'exec', self.issuer, *command])
        self.pair = model.validate_pair(model.strict_json(result.stdout))
        self.save('pair-public.json', self.pair)
        for node in self.nodes.values():
            self.stage_node_secrets(node)
            node.write(model.config_path(node.key) + '/network-access', b'network\n')
            node.inside('mkdir', '-p', '/var/lib/agentvm/logs')
            node.call(['docker', 'exec', '-d', node.name, 'sh', '-c',
                       'exec /var/lib/agentpair/tools/fixture serve --directory "$1" --listen 127.0.0.1:0 --handoff /config/acme-enrollment.json --duration 10m > /var/lib/agentvm/logs/issuer.log 2>&1', 'sh', model.config_path(node.key)])
            node.wait(lambda: node.python("import os; assert os.path.isfile('/config/acme-enrollment.json')", check=False).returncode == 0, 30)
            node.start_agent('enrollment')
            node.wait(lambda: node.python("import json,os,sys; p=sys.argv[1]; assert json.load(open(p+'/provisioning.json')).get('enrolled'); assert os.path.isfile(p+'/.vm-fixture/issued.json')", model.config_path(node.key), check=False).returncode == 0, 90)
            issued = node.read(model.config_path(node.key) + '/.vm-fixture/issued.json', 64 << 10)
            model.strict_json(issued)
            library.stage.write_new(self.out / ('issued-' + node.key + '.json'), issued)
            self.call(['docker', 'exec', '-i', self.issuer, 'python3', '-c', WRITE, model.config_path(node.key) + '/.vm-fixture/issued.json', str(len(issued)), model.digest(issued), '600'], input_data=issued)
        public = self.call(['docker', 'exec', self.issuer, model.TOOLS + '/pair-fixture', 'peers', '--directory', model.PAIR])
        self.peers = model.validate_peers(self.pair, model.strict_json(public.stdout))
        self.save('pair-peers.json', self.peers)
        # Keeping the initializer until ordinary cleanup lets failed later phases
        # preserve only public receipts. Its private volume is never exported.

    def commission(self):
        model.require(self.peers is not None, 'both public enrollment proofs required before commissioning')
        self.step('commission')
        addresses = {}
        for key, node in self.nodes.items():
            info = model.strict_json(self.call(['docker', 'inspect', node.name]).stdout, 1 << 20)
            addresses[key] = info[0]['NetworkSettings']['Networks'][self.network]['IPAddress']
        self.save('observed-addresses.json', addresses)
        self.addresses = addresses
        for key, node in self.nodes.items():
            node.stop_agent()
            cfg = model.commission(self.pair, self.peers, key, self.inputs['run_id'])
            config_raw = model.json_bytes(cfg)
            node.write(model.config_path(key) + '/world.json', config_raw)
            node.world_config_sha256 = model.digest(config_raw)
            node.clock_raw, _ = control.sample_clock(self.inputs['run_id'], 0)
            node.mesh_raw = model.json_bytes(model.mesh_configuration(self.pair, key, addresses))
            node.write(model.config_path(key) + '/simclock.json', node.clock_raw)
            node.write(model.config_path(key) + '/local-mesh.json', node.mesh_raw)
            node.write(model.config_path(key) + '/nan-mesh.json', model.json_bytes({'participate': True, 'roam': False, 'shareUplink': False}))
            self.save('commission-' + key + '.json', cfg)
            node.inside(model.TOOLS + '/initializer', '--config', model.config_path(key) + '/world.json', timeout=45)
        for node in self.nodes.values():
            node.start_agent('commissioned')

    def deploy(self):
        model.require(self.runtime_state == 'commission', 'commissioning must precede app deployment')
        self.step('deploy')
        # Recheck both commissioned starts before either node gets app volumes,
        # a build or a CLI deployment. Apps-list alone only proved agent RPC.
        for node in self.nodes.values():
            node.check_world_startup()
        for node in self.nodes.values():
            self.deploy_node(node)
        # Native readiness and nonempty repair checks belong to the subsequent
        # scenario. A first query before that handshake can legitimately refuse.

    def deploy_node(self, node):
        key = node.key
        node.check_world_startup()
        node.generation += 1
        node.command_sequence = 0
        node.task_state = None
        names = model.volume_names(self.inputs['run_id'], key, node.generation)
        suffix = '' if node.generation == 1 else '-generation-' + str(node.generation)
        metadata = node.python(control.VOLUME_DIRECTORIES, *names.values()).stdout
        self.save('volume-directories-' + key + suffix + '.json', control.volume_receipt(metadata, list(names.values())))
        node.write('/var/lib/wendy/volumes/' + names['config'] + '/probe.json', model.json_bytes(model.probe_config(self.pair, self.inputs['run_id'], key)))
        project = model.PROJECT
        if node.generation != 1:
            project += '/generation-' + str(node.generation)
            node.python(library.CREATE_STAGE, project)
        node.write(project + '/wendy.json', model.json_bytes(model.app_manifest(self.pair, self.inputs['run_id'], key, node.generation)), 0o644)
        node.write(project + '/Dockerfile', b'FROM scratch\nCOPY probe /probe\nENTRYPOINT ["/probe","--config","/config/probe.json","--commands","/control","--receipts","/proofs"]\n', 0o644)
        node.inside('cp', model.TOOLS + '/probe', project + '/probe')
        node.cli('run', '--prefix', project, '--builder', 'buildkit', '--chunking', 'force', '--detach', '--skip-cloud-registration', '--yes')
        self.save('task-' + key + suffix + '.json', node.observe_task())
        node.wait(lambda: node.python("import os,sys; assert os.path.isfile(sys.argv[1])", '/var/lib/wendy/volumes/' + names['proofs'] + '/session.json', check=False).returncode == 0, 15)
        session = model.strict_json(node.read('/var/lib/wendy/volumes/' + names['proofs'] + '/session.json'))
        cfg = model.probe_config(self.pair, self.inputs['run_id'], key)
        model.require(session.get('status') == 'awaiting_command' and session.get('run_id') == cfg['run_id'] and session.get('device') == cfg['device'] and session.get('peer_device') == cfg['peer_device'], 'probe session differs')
        self.save('probe-session-' + key + suffix + '.json', session)

    def replication(self):
        import scenario
        return scenario.execute(self)

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

    def snapshot_outputs(self, node, identity):
        saved = {}
        if self.trace_rpc:
            saved['telemetry'] = self.snapshot_telemetry(node, identity)
        for phase in ('enrollment', 'commissioned', 'restart'):
            name = 'agent-' + phase + '.log'
            self.current_container_id(node.name, identity)
            saved[name] = node.snapshot_file('/var/lib/agentvm/logs/' + name, self.out / 'private-runtime-logs' / node.key / name, 8 << 20, container=identity)
        for generation in range(1, node.generation + 1):
            names = model.volume_names(self.inputs['run_id'], node.key, generation)
            for name in ('session.json', 'result.json', *[f'{i:06d}.json' for i in range(1, node.command_counts.get(generation, 0) + 1)]):
                self.current_container_id(node.name, identity)
                suffix = 'generation-' + str(generation) + '/' + name
                saved[suffix] = node.snapshot_file('/var/lib/wendy/volumes/' + names['proofs'] + '/' + name, self.out / 'cleanup-probe' / node.key / suffix, 512 << 10, container=identity)
        self.save('snapshots-' + node.key + '.json', saved)

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

    def execute(self):
        result = {'passed': False, 'requested_gate': self.phase, 'simulation_only': True, 'replication_acceptance': False}
        previous = {}
        def interrupted(signum, _):
            raise library.InterruptedRun(signum)
        for signum in (signal.SIGINT, signal.SIGTERM):
            previous[signum] = signal.signal(signum, interrupted)
        try:
            try:
                self.setup(); self.enroll()
                if self.phase in ('commission', 'deploy', 'replication'):
                    self.commission()
                if self.phase in ('deploy', 'replication'):
                    self.deploy()
                if self.phase == 'replication':
                    result['replication'] = self.replication()
                    # Durable tombstone/floor receipt is a separate incomplete
                    # gate; successful scalar transfer does not close it.
                result['gate_completed'] = True
            except BaseException as error:
                result.update(gate_completed=False, error=type(error).__name__, failed_phase=self.runtime_state)
                # Unknown writes/enrollment/initialization remain unknown. No retry.
            finally:
                cleanup = self.cleanup()
                result['cleanup'] = cleanup
                result['passed'] = result.get('gate_completed', False) and cleanup['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'requested gate or cleanup did not pass; private receipts retained')
            return result
        finally:
            for signum, handler in previous.items():
                signal.signal(signum, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inputs', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--phase', choices=('enrollment', 'commission', 'deploy', 'replication'), default='enrollment')
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.inputs, args.output, phase=args.phase, trace_rpc=args.trace_rpc).execute()


if __name__ == '__main__':
    main()
