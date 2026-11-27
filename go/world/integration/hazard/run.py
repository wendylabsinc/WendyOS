#!/usr/bin/env python3
"""Ordinary native hazard deployment using run I's isolated lifecycle helpers."""
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

base = libraries.load_runtime(model, stage, control)


class Runner(base.Runner):
    def projects(self):
        self.step('projects')
        for key, node in self.nodes.items():
            node.write(stage.REMOTE + '/project/wendy.json', model.json_bytes(model.app_manifest(self.pair, self.inputs['run_id'], key)), 0o644)
            for service in model.SERVICES[key]:
                names = list(model.volume_names(self.inputs['run_id'], key, service).values())
                raw = node.python(control.VOLUME_DIRECTORIES, *names).stdout
                self.save('volumes-' + key + '-' + service + '.json', control.volume_receipt(raw, names))
        a = self.nodes['a']
        paths = [a.service_volume(s) for s in ('scene', *model.SENSORS)]
        a.python("import os,secrets,sys\npaths=sys.argv[1:]; values=[secrets.token_urlsafe(32),secrets.token_urlsafe(32)]\n"
                 "def put(p,v):\n f=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)\n with os.fdopen(f,'w') as s:s.write(v+'\\n');s.flush();os.fsync(s.fileno())\n"
                 "for p in paths:put(p+'/scene-read-token',values[0])\nput(paths[0]+'/scene-control-token',values[1])\nprint('source credentials created in owned node')", *paths)

    def deploy(self):
        self.step('apps')
        for key, node in self.nodes.items():
            node.check_world_startup()
            for service in model.SERVICES[key]:
                if service == 'scene':
                    continue
                task = self.deploy_service(node, service)
                if service in model.SENSORS:
                    cfg = model.capture_config(service, self.inputs['run_id'], self.scene, self.scene_url)
                elif service == 'fusion':
                    cfg = model.fusion_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                else:
                    cfg = model.reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
                node.write(node.service_volume(service) + '/config.json', model.json_bytes(cfg))
        sockets = [v['socket_directory'] for k, v in self.tasks.items() if k != 'a/scene']
        count = len(model.SENSORS) + 2
        model.require(len(sockets) == count and len(set(sockets)) == count, 'distinct ordinary World mounts required')
        for key, node in self.nodes.items():
            for service in model.SERVICES[key]:
                if service != 'scene':
                    node.write(node.service_volume(service) + '/go', b'')

    def execute(self):
        if __package__:
            from . import scenario
        else:
            import scenario
        result = {'passed': False, 'simulation_only': True, 'remote_raw_input_verification': False}
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
                cleanup = self.cleanup()
                result['cleanup'] = cleanup
                result['passed'] = result.get('gate_completed', False) and cleanup['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'hazard scenario or owned cleanup failed; receipts retained')
            return result
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, trace_rpc=args.trace_rpc).execute()


if __name__ == '__main__':
    main()
