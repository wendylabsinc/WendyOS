#!/usr/bin/env python3
"""Split ordinary gas capture and multimodal fusion across two native Agents."""
import argparse
import ipaddress
import os
from pathlib import Path
import signal
import subprocess

if __package__:
    from . import libraries, model, stage, control
else:
    import libraries
    import model
    import stage
    import control

base = libraries.load('distributedfusion_hazard_run', libraries.HERE.parent / 'hazard/run.py',
                     {'libraries': libraries, 'model': model, 'stage': stage, 'control': control})

TOKEN_SEND = """import os,stat,sys
f=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(f,'rb') as s:
 st=os.fstat(s.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and st.st_nlink==1 and not st.st_mode&0o077
 raw=s.read(258);assert 33<=len(raw)<=257 and raw.endswith(b'\\n') and not any(c in raw[:-1] for c in b' \\r\\n\\t')
 sys.stdout.buffer.write(raw)
"""
TOKEN_RECEIVE = """import os,sys
raw=sys.stdin.buffer.read(258);assert 33<=len(raw)<=257 and raw.endswith(b'\\n') and not any(c in raw[:-1] for c in b' \\r\\n\\t')
f=os.open(sys.argv[1],os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(f,'wb') as s:s.write(raw);s.flush();os.fsync(s.fileno())
print('read credential received')
"""


class Runner(base.Runner):
    def projects(self):
        self.step('projects')
        for key, node in self.nodes.items():
            node.write(stage.REMOTE + '/project/wendy.json', model.json_bytes(model.app_manifest(self.pair, self.inputs['run_id'], key)), 0o644)
            for service in model.SERVICES[key]:
                names = list(model.volume_names(self.inputs['run_id'], key, service).values())
                self.save('volumes-' + key + '-' + service + '.json',
                          control.volume_receipt(node.python(control.VOLUME_DIRECTORIES, *names).stdout, names))
        a = self.nodes['a']
        paths = [a.service_volume(s) for s in ('scene', *model.LOCAL_SENSORS)]
        a.python("import os,secrets,sys\npaths=sys.argv[1:];values=[secrets.token_urlsafe(32),secrets.token_urlsafe(32)]\n"
                 "def put(p,v):\n f=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)\n with os.fdopen(f,'w') as s:s.write(v+'\\n');s.flush();os.fsync(s.fileno())\n"
                 "for p in paths:put(p+'/scene-read-token',values[0])\nput(paths[0]+'/scene-control-token',values[1])\nprint('source credentials created in owned node')", *paths)
        self.transfer_read_token()

    def transfer_read_token(self):
        a, b = self.nodes['a'], self.nodes['b']
        sender = ['docker', 'exec', a.target(), 'python3', '-c', TOKEN_SEND,
                  a.service_volume('scene') + '/scene-read-token']
        receiver = ['docker', 'exec', '-i', b.target(), 'python3', '-c', TOKEN_RECEIVE,
                    b.service_volume('gas-1') + '/scene-read-token']
        source = target = None
        try:
            source = subprocess.Popen(sender, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            target = subprocess.Popen(receiver, stdin=source.stdout, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
            source.stdout.close()
            output, _ = target.communicate(timeout=10)
            source_code = source.wait(timeout=5)
            model.require(source_code == 0 and target.returncode == 0 and output == b'read credential received\n', 'read credential transfer refused')
        finally:
            for process in (source, target):
                if process is not None and process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=5)
        self.save('source-read-credential-transfer.json', {'source': 'a', 'receiver': 'b/gas-1',
                  'source_exit': source_code, 'receiver_exit': target.returncode,
                  'host_payload_retained': False, 'control_credential_transferred': False})

    def addresses(self):
        result = {}
        for key, node in self.nodes.items():
            rows = model.strict_json(self.call(['docker', 'inspect', node.target()]).stdout, 1 << 20)
            value = rows[0]['NetworkSettings']['Networks'][self.network]['IPAddress']
            model.require(str(ipaddress.IPv4Address(value)) == value, 'observed owned IPv4 required')
            result[key] = value
        return result

    def source_scene(self):
        super().source_scene()
        self.node_addresses = self.addresses()
        self.save('observed-addresses.json', self.node_addresses)
        a, b = self.nodes['a'], self.nodes['b']
        code = (libraries.HERE / 'relay.py').read_text()
        a.call(['docker', 'exec', '-d', a.target(), 'python3', '-c', code,
                '--bind', self.node_addresses['a'], '--port', '8092', '--origin', self.scene_url])
        self.remote_scene_url = 'http://' + self.node_addresses['a'] + ':8092'
        def ready():
            response = b.python(control.HTTP, self.remote_scene_url, '/v1/identity',
                                b.service_volume('gas-1') + '/scene-read-token', '-', check=False, timeout=6)
            if response.returncode:
                return False
            model.require(model.strict_json(response.stdout) == self.scene, 'remote source identity differs')
            return True
        b.wait(ready, 10)
        self.save('source-relay.json', {'version': 1, 'source_task': self.tasks['a/scene'],
                  'origin': self.scene_url, 'relay': self.remote_scene_url, 'same_identity': True,
                  'allowed_paths': ['/v1/identity', '/v1/clock', '/v1/captures/gas-1'],
                  'world_transport': False, 'control_forwarding': False})

    def commission(self):
        self.step('commission')
        model.require(self.addresses() == self.node_addresses, 'owned source addresses changed')
        for key, node in self.nodes.items():
            node.stop_agent()
            url = self.scene_url if key == 'a' else self.remote_scene_url
            token_service = 'scene' if key == 'a' else 'gas-1'
            cfg = model.commission(key, self.pair, self.peers, self.inputs['run_id'], self.scene,
                                  scene_url=url, read_token_file=node.service_volume(token_service) + '/scene-read-token')
            raw = model.json_bytes(cfg)
            node.write(model.config_path(key) + '/world.json', raw)
            node.world_config_sha256 = model.digest(raw)
            node.mesh_raw = model.json_bytes(model.mesh_configuration(self.pair, key, self.node_addresses))
            node.write(model.config_path(key) + '/local-mesh.json', node.mesh_raw)
            node.write(model.config_path(key) + '/nan-mesh.json', model.json_bytes({'participate': True, 'roam': False, 'shareUplink': False}))
            self.save('commission-' + key + '.json', cfg)
            node.inside(model.TOOLS + '/initializer', '--config', model.config_path(key) + '/world.json', timeout=45)
        for node in self.nodes.values():
            node.start_agent('commissioned')
        after = self.nodes['a'].task('scene')
        model.require(after == self.tasks['a/scene'] and model.strict_json(self.nodes['a'].scene_request('/v1/identity')) == self.scene, 'single source changed across commissioning')
        self.save('scene-continuity.json', {'same_task': True, 'same_identity': True, 'task': after})

    def deploy(self):
        self.step('apps')
        for key, node in self.nodes.items():
            node.check_world_startup()
            for service in model.SERVICES[key]:
                if service == 'scene':
                    continue
                task = self.deploy_service(node, service)
                if service in model.SENSORS:
                    cfg = model.capture_config(service, self.inputs['run_id'], self.scene,
                                               self.scene_url if key == 'a' else self.remote_scene_url)
                elif service == 'fusion':
                    cfg = model.fusion_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                else:
                    cfg = model.reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                node.write(node.service_volume(service) + '/config.json', model.json_bytes(cfg))
        sockets = [v['socket_directory'] for k, v in self.tasks.items() if k != 'a/scene']
        model.require(len(sockets) == len(model.SENSORS) + 2 and len(set(sockets)) == len(sockets), 'distinct ordinary mounts required')
        for key, node in self.nodes.items():
            for service in model.SERVICES[key]:
                if service != 'scene':
                    node.write(node.service_volume(service) + '/go', b'')

    def execute(self):
        if __package__:
            from . import scenario
        else:
            import scenario
        result = {'passed': False, 'simulation_only': True, 'remote_raw_input_verification_at_reader': False}
        prior = {}
        def interrupted(signum, _):
            raise libraries.runtime.InterruptedRun(signum)
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
                result['cleanup'] = self.cleanup()
                result['passed'] = result.get('gate_completed', False) and result['cleanup']['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'distributed fusion or owned cleanup failed; receipts retained')
            return result
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, trace_rpc=args.trace_rpc).execute()
