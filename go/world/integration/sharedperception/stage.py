#!/usr/bin/env python3
"""Prepare shared-source contexts using the existing offline stage machinery."""
import argparse
from pathlib import Path

from integration.remotemediafusion import libraries
from integration.sharedperception import model

_base = libraries.load('sharedperception_hazard_stage', libraries.HERE.parent / 'hazard/stage.py',
                       {'libraries': libraries, 'model': model})
_people = libraries.people_stage
ROOT, RUNTIME = _base.ROOT, _base.RUNTIME
TOOLS = (*_base.TOOLS, 'people', 'people-reader')
ROOTFS = {**_base.ROOTFS, 'pose': _people.ROOTFS['pose']}
SERVICES = model.SERVICES
REMOTE = '/var/lib/sharedperception-stage'
_base.TOOLS, _base.ROOTFS, _base.SERVICES, _base.REMOTE = TOOLS, ROOTFS, SERVICES, REMOTE
copied, inventory, validate = _base.copied, _base.inventory, _base.validate
_original_inputs, _original_dockerfile = _base.inputs, _base.dockerfile


def inputs(raw):
    plan = model.strict_json(raw)
    model.require(isinstance(plan, dict) and set(plan) == {
        'version', 'run_id', 'runtime_image', 'binaries', 'rootfs', 'anchor_unix_ns', 'model'},
        'exact shared perception inputs required')
    _original_inputs(model.json_bytes({key: value for key, value in plan.items() if key != 'model'}))
    pose = plan['model']
    model.require(isinstance(pose, dict) and set(pose) == {'model_id', 'model_version', 'model_sha256'} and
                  all(model._people._text(pose[key], 256) for key in ('model_id', 'model_version')) and
                  model.digest_shape(pose['model_sha256']), 'exact pose model identity required')
    return plan


def dockerfile(service, run_id, anchor):
    if service == 'people':
        return _people.dockerfile(service, run_id, anchor)
    raw = _original_dockerfile(service, run_id, anchor)
    if service == 'scene':
        raw = raw.replace(b'"integration.hazard.scene"', b'"integration.sharedperception.scene"')
    return raw


_base.inputs, _base.dockerfile = inputs, dockerfile


def prepare(raw, output):
    # Extend each context before the shared inventory seals it. Do not reimplement
    # file copying, tool pinning, output ownership or stage validation.
    plan = model.strict_json(raw)  # _base.prepare validates every pin before invoking finish.
    def finish(directory):
        if directory.name == 'a':
            context = directory / 'project/services'
            source = ROOT / 'integration/sharedperception/scene.py'
            copied(source, context / 'scene/source/integration/sharedperception/scene.py', mode=0o644)
            pose = plan['rootfs']['pose']
            copied(pose['path'], context / 'people/rootfs.tar', pose['sha256'])
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
    print('shared perception stage verified')
