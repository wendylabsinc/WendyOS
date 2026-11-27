#!/usr/bin/env python3
"""Prepare an isolated split-source app stage without starting a runtime."""
import argparse
from pathlib import Path

if __package__:
    from . import libraries, model
else:
    import libraries
    import model

_base = libraries.load('distributedfusion_hazard_stage', libraries.HERE.parent / 'hazard/stage.py',
                       {'libraries': libraries, 'model': model})
_base.REMOTE = '/var/lib/distributedfusion-stage'
for _name in ('ROOT', 'TOOLS', 'RUNTIME', 'ROOTFS', 'SERVICES', 'REMOTE', 'inputs', 'dockerfile',
              'copied', 'inventory', 'prepare', 'validate'):
    globals()[_name] = getattr(_base, _name)


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
    print('distributed fusion stage verified')
