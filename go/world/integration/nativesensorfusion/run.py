#!/usr/bin/env python3
"""Native SensorLink camera B into ordinary fusion and a second Agent reader."""
import argparse
from pathlib import Path
import signal

if __package__:
    from . import libraries, model, stage, control
else:
    import libraries
    import model
    import stage
    import control

_hazard = libraries.load('nativesensorfusion_hazard_runtime', libraries.HERE.parent / 'hazard/run.py',
                        {'libraries': libraries, 'model': model, 'stage': stage, 'control': control})
base = _hazard.base
SOURCE_OUTPUT = model.TOOLS + '/native-source-output'
SOURCE_CONFIG = model.TOOLS + '/native-source.json'


class Runner(_hazard.Runner):
    def native_pair(self, *args):
        socket = libraries.runtime.base.ENV['WENDY_AGENT_SOCKET']
        return self.nodes['a'].inside('env', 'WENDY_AGENT_SOCKET=' + socket,
                                      model.TOOLS + '/native-pair', *args, timeout=8)

    def projects(self):
        self.step('projects')
        for key, node in self.nodes.items():
            node.write(stage.REMOTE + '/project/wendy.json', model.json_bytes(model.app_manifest(self.pair, self.inputs['run_id'], key)), 0o644)
            for service in model.SERVICES[key]:
                names = list(model.volume_names(self.inputs['run_id'], key, service).values())
                self.save('volumes-' + key + '-' + service + '.json',
                          control.volume_receipt(node.python(control.VOLUME_DIRECTORIES, *names).stdout, names))
        a = self.nodes['a']
        paths = [a.service_volume(s) for s in ('scene', *model.HTTP_SENSORS)]
        a.python("import os,secrets,sys\npaths=sys.argv[1:]; read,control,metadata=[secrets.token_urlsafe(32) for _ in range(3)]\n"
                 "def put(p,v):\n f=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)\n with os.fdopen(f,'w') as s:s.write(v+'\\n');s.flush();os.fsync(s.fileno())\n"
                 "for p in paths[:-1]:put(p+'/scene-read-token',read)\n"
                 "put(paths[0]+'/scene-control-token',control)\nput(paths[-1]+'/metadata-read-token',metadata)\n"
                 "print('owned source tokens created')", *paths, a.service_volume(model.NATIVE_CAMERA))

    def native_source(self):
        self.step('native-source')
        a = self.nodes['a']
        self.source_id = model.native_source_id(self.pair['nodes'][0]['public']['principal'])
        addresses = model.strict_json((self.out / 'observed-addresses.json').read_bytes())
        self.metadata_url = 'http://' + addresses['a'] + ':50061'
        cfg = model.native_source_config(self.inputs['run_id'], self.scene,
            scene_url=self.scene_url, scene_token_file=a.service_volume('scene') + '/scene-read-token',
            metadata_token_file=a.service_volume(model.NATIVE_CAMERA) + '/metadata-read-token',
            output_directory=SOURCE_OUTPUT)
        a.python(libraries.runtime.CREATE_STAGE, SOURCE_OUTPUT)
        a.write(SOURCE_CONFIG, model.json_bytes(cfg))
        self.save('native-source-config.json', cfg)
        # The source runs in A's owned namespace and reads its existing native
        # enrollment. Nothing mounts its keys into an ordinary app container.
        a.call(['docker', 'exec', '-d', a.target(), model.TOOLS + '/app-runner',
                '--directory', SOURCE_OUTPUT, '--duration', '210s', '--stdout-limit', str(4 << 20),
                '--stderr-limit', str(1 << 20), '--', model.TOOLS + '/native-source', 'serve', '--config', SOURCE_CONFIG])
        expected = {'version': 1, 'ready': True, 'tcp_listen': '127.0.0.1:50060',
                    'metadata_listen': '0.0.0.0:50061'}
        def ready():
            response = a.python("import json,sys; print(json.dumps(json.load(open(sys.argv[1]))))",
                                SOURCE_OUTPUT + '/ready.json', check=False)
            if response.returncode:
                return False
            model.require(model.strict_json(response.stdout) == expected, 'native source ready receipt differs')
            return True
        a.wait(ready, 10)
        self.save('native-source-ready.json', expected)
        paired = self.native_pair('pair', '--principal',
                                  self.pair['nodes'][0]['public']['principal'], '--address', '127.0.0.1:50060')
        reply = model.strict_json(paired.stdout, 64 << 10)
        validate_pairing(reply, self.pair['nodes'][0]['public']['principal'])
        self.save('native-pairing.json', reply)

    def deploy(self):
        self.step('apps')
        for key, node in self.nodes.items():
            node.check_world_startup()
            for service in model.SERVICES[key]:
                if service == 'scene':
                    continue
                task = self.deploy_service(node, service)
                if service == model.NATIVE_CAMERA:
                    cfg = model.native_bridge_config(self.inputs['run_id'], self.scene,
                                source_id=self.source_id, metadata_url=self.metadata_url)
                elif service in model.HTTP_SENSORS:
                    cfg = model.capture_config(service, self.inputs['run_id'], self.scene, self.scene_url)
                elif service == 'fusion':
                    cfg = model.fusion_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                else:
                    cfg = model.reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                node.write(node.service_volume(service) + '/config.json', model.json_bytes(cfg))
        sockets = [v['socket_directory'] for k, v in self.tasks.items() if k != 'a/scene']
        model.require(len(sockets) == 7 and len(set(sockets)) == 7, 'distinct ordinary app mounts required')
        for key, node in self.nodes.items():
            for service in model.SERVICES[key]:
                if service not in ('scene', model.NATIVE_CAMERA):
                    node.write(node.service_volume(service) + '/go', b'')

    def snapshot_outputs(self, node, identity):
        # Preserve normal app outputs plus the private helper's public results.
        result = super().snapshot_outputs(node, identity)
        if node.key != 'a':
            return result
        files = [('ready.json', 4096), ('stdout.json', 4 << 20),
                 ('stderr.log', 1 << 20), ('exit.json', 4096)]
        files += [(f'frame-{number:06d}' + suffix, limit) for number in (1, 2)
                  for suffix, limit in (('.jpg', 8 << 20), ('.json', 64 << 10))]
        saved = {}
        for name, limit in files:
            try:
                self.current_container_id(node.name, identity)
                source = SOURCE_OUTPUT + '/' + name
                present = node.python('import os,sys\ntry:os.lstat(sys.argv[1])\n'
                                      "except FileNotFoundError:print('not_created')\nelse:print('present')",
                                      source, check=False)
                if present.returncode == 0 and present.stdout == b'not_created\n':
                    saved[name] = {'status': 'not_created'}
                else:
                    model.require(present.returncode == 0 and present.stdout == b'present\n',
                                  'native source artifact inspection unavailable')
                    self.current_container_id(node.name, identity)
                    saved[name] = node.snapshot_file(source, self.out / 'native-source' / name,
                                                    limit, container=identity)
            except Exception:
                saved[name] = {'status': 'capture_unavailable'}
        self.save('native-source-snapshots.json', saved)
        return result

    def execute(self):
        if __package__:
            from . import scenario
        else:
            import scenario
        result = {'passed': False, 'simulation_only': True, 'raw_inputs_independently_verified_at_receiver': False}
        prior = {}
        def interrupted(signum, _):
            raise libraries.runtime.InterruptedRun(signum)
        for signum in (signal.SIGTERM, signal.SIGINT):
            prior[signum] = signal.signal(signum, interrupted)
        try:
            try:
                self.setup(); self.enroll(); self.projects(); self.source_scene(); self.commission()
                self.native_source(); self.deploy()
                result['observations'] = scenario.execute(self)
                result['gate_completed'] = True
            except BaseException as error:
                result.update(gate_completed=False, error=type(error).__name__, failed_phase=self.runtime_state)
            finally:
                result['cleanup'] = self.cleanup()
                result['passed'] = result.get('gate_completed', False) and result['cleanup']['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'native sensor fusion or owned cleanup failed; receipts retained')
            return result
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)


def validate_pairing(reply, principal):
    model.require(isinstance(reply, dict) and set(reply) == {'pairing'}, 'native pairing reply required')
    p = reply['pairing']
    model.require(p.get('source_principal_uri') == principal and p.get('source_asset_id') == 0 and
                  p.get('transport') == 'tcp' and p.get('sensor_allowlist') == ['camera-b'] and
                  p.get('connected') is False, 'native pairing source or channel differs')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, trace_rpc=args.trace_rpc).execute()
