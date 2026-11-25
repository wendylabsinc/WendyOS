#!/usr/bin/env python3
"""Prepare isolated native-camera app contexts; never starts a runtime."""
import argparse
from pathlib import Path

if __package__:
    from . import libraries, model
else:
    import libraries
    import model

_base = libraries.load('nativesensorfusion_hazard_stage', libraries.HERE.parent / 'hazard/stage.py',
                       {'libraries': libraries, 'model': model})
ROOT, RUNTIME, ROOTFS = _base.ROOT, _base.RUNTIME, _base.ROOTFS
SERVICES, REMOTE = model.SERVICES, '/var/lib/nativesensorfusion-stage'
TOOLS = (*_base.TOOLS, 'sensorlink', 'native-source', 'native-pair')
_base.TOOLS, _base.REMOTE = TOOLS, REMOTE
inputs, copied, inventory, validate = _base.inputs, _base.copied, _base.inventory, _base.validate


def dockerfile(service, run_id, anchor):
    if service != model.NATIVE_CAMERA:
        raw = _base.dockerfile(service, run_id, anchor)
        if service == 'scene':
            prefix, entry = raw.decode().split('ENTRYPOINT ', 1)
            argv = model.strict_json(entry.encode())
            argv.extend(['--capture-resolution-ns', '1000'])
            return (prefix + 'ENTRYPOINT ' + model.json_bytes(argv).decode()).encode()
        return raw
    wrapper = ['/app-runner', '--directory', '/proofs', '--duration', '600s',
               '--stdout-limit', str(32 << 20), '--stderr-limit', str(1 << 20),
               '--start-file', '/config/go', '--', '/service', '--mode', 'native-bridge',
               '--config', '/config/config.json']
    return ('FROM scratch\nCOPY app-runner /app-runner\nCOPY sensorlink /service\nUSER 0:0\nENTRYPOINT ' +
            model.json_bytes(wrapper).decode()).encode()


def prepare(raw, output):
    plan = inputs(raw)
    output = libraries.stage.safe_output(Path(output), ROOT / '.local')
    libraries.stage.write_new(output / 'inputs.json', model.json_bytes(plan))
    for node, services in SERVICES.items():
        directory = output / node
        directory.mkdir(mode=0o700)
        for name, row in plan['binaries'].items():
            copied(row['path'], directory / 'tools' / name, row['sha256'], 0o755)
        for service in services:
            context = directory / 'project' / 'services' / service
            libraries.stage.write_new(context / 'Dockerfile', dockerfile(service, plan['run_id'], plan['anchor_unix_ns']), 0o644)
            app = plan['binaries']['app-runner']
            copied(app['path'], context / 'app-runner', app['sha256'], 0o755)
            if service == 'scene':
                row = plan['rootfs']['scene']
                copied(row['path'], context / 'rootfs.tar', row['sha256'])
                sources = [*sorted((ROOT / 'sim').glob('*.py')), ROOT / 'integration/hazard/scene.py',
                           ROOT / 'examples/collaborative-office/office.xml', ROOT / 'examples/collaborative-office/scene.json']
                for source in sources:
                    copied(source, context / 'source' / source.relative_to(ROOT), mode=0o644)
            else:
                name = ('sensorlink' if service == model.NATIVE_CAMERA else
                        'capture' if service in model.HTTP_SENSORS else service)
                row = plan['binaries'][name]
                copied(row['path'], context / name, row['sha256'], 0o755)
        inventory(directory)
    libraries.stage.write_new(output / 'stage.json', model.json_bytes({'version': 1,
        'inputs_sha256': model.digest((output / 'inputs.json').read_bytes()),
        'nodes': {node: model.digest((output / node / 'files.json').read_bytes()) for node in SERVICES}}))
    return validate(output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'validate'))
    parser.add_argument('--inputs', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.action == 'prepare':
        model.require(args.inputs is not None, '--inputs required')
        prepare(model.read_regular(str(args.inputs.resolve()), 256 << 10), args.output.resolve())
    else:
        validate(args.output.resolve())
    print('native sensor fusion stage verified')
