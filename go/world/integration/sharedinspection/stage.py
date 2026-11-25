"""Prepare an isolated continuous office source and signed mission services."""
import argparse
from pathlib import Path

from integration.remotemediafusion import libraries
from integration.sharedinspection import model

_base = libraries.load('sharedinspection_hazard_stage', libraries.HERE.parent / 'hazard/stage.py',
                       {'libraries': libraries, 'model': model})
_people = libraries.people_stage
ROOT, RUNTIME = _base.ROOT, _base.RUNTIME
TOOLS = (*_base.TOOLS, 'people', 'people-reader', 'mission-gateway', 'atlas-executor', 'mission-commissioner', 'viewer')
ROOTFS = {**_base.ROOTFS, 'pose': _people.ROOTFS['pose']}
SERVICES = model.SERVICES
REMOTE = '/var/lib/sharedinspection-stage'
_base.TOOLS, _base.ROOTFS, _base.SERVICES, _base.REMOTE = TOOLS, ROOTFS, SERVICES, REMOTE
copied, inventory, validate = _base.copied, _base.inventory, _base.validate
_original_inputs, _original_dockerfile = _base.inputs, _base.dockerfile


def inputs(raw):
    plan = model.strict_json(raw)
    model.require(isinstance(plan, dict) and set(plan) == {
        'version', 'run_id', 'runtime_image', 'binaries', 'rootfs', 'anchor_unix_ns', 'model'},
        'exact shared inspection inputs required')
    _original_inputs(model.json_bytes({key: value for key, value in plan.items() if key != 'model'}))
    pose = plan['model']
    model.require(isinstance(pose, dict) and set(pose) == {'model_id', 'model_version', 'model_sha256'} and
                  all(model._shared._people._text(pose[key], 256) for key in ('model_id', 'model_version')) and
                  model.digest_shape(pose['model_sha256']), 'exact pose model identity required')
    return plan


def dockerfile(service, run_id, anchor):
    if service == 'people':
        return _people.dockerfile(service, run_id, anchor)
    raw = _original_dockerfile(service, run_id, anchor)
    if service == 'scene':
        raw = raw.replace(b'"integration.hazard.scene"', b'"integration.sharedperception.scene"')
        raw = raw.replace(b'"--bind"', b'"--inspection","--inspection-defer-start","--inspection-fixed-capture-hz","1","--bind"')
    if service == 'atlas-executor':
        raw = raw.replace(b'COPY atlas-executor /service', b'COPY atlas-executor /executor\nCOPY mission-commissioner /service')
        raw = raw.replace(b'["/service","--config","/config/config.json"]', b'["/service","executor","--config","/config/launch.json"]')
        # ENTRYPOINT includes the app-runner prefix, so replace the command tail.
        raw = raw.replace(b'"/service","--config","/config/config.json"', b'"/service","executor","--config","/config/launch.json"')
    return raw


_base.inputs, _base.dockerfile = inputs, dockerfile


def prepare(raw, output):
    plan = model.strict_json(raw)
    def finish(directory):
        if directory.name == 'a':
            contexts = directory / 'project/services'
            copied(ROOT / 'integration/sharedperception/scene.py', contexts / 'scene/source/integration/sharedperception/scene.py', mode=0o644)
            pose = plan['rootfs']['pose']
            copied(pose['path'], contexts / 'people/rootfs.tar', pose['sha256'])
            helper = plan['binaries']['mission-commissioner']
            copied(helper['path'], contexts / 'atlas-executor/mission-commissioner', helper['sha256'], 0o755)
        return inventory(directory)
    previous = _base.inventory
    try:
        _base.inventory = finish
        return _base.prepare(raw, output)
    finally:
        _base.inventory = previous


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
    print('shared inspection stage verified')
