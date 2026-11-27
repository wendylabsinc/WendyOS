#!/usr/bin/env python3
"""Bind the kernel comparison to the exact public Builder recipe and patch."""
import argparse
import hashlib
import json
import re
from pathlib import Path
import subprocess

PINS = {
    'upstream_revision': '0f9ee86760b7f2bea174b7e3e7a1d38845da0ab4',
    'builder_revision': '21bf838798dc16ac2bdbf3e7d1c0e0d88c74fd65',
    'source_sha256': 'a3e78c9ee061e5ce81b0ea45bb1730ce531b5ece7194f56a47d831ec2d009c2b',
    'patched_source_sha256': '35942b3c7461a11c69bdf22b1748ade248ad80b76ac7857ff75da697552576a9',
    'recipe_sha256': 'b70285d1b139f1cfa69f3aad0733467c1be373cf88de7abf2583caa3cba57135',
    'fresh_patch_sha256': '504b7cce2c2952b32091a56bdbc8ee33472cd8d24f85374a4a88e98ab84e7601',
    'patch_sha256': 'b27ee9278d8b296e6add0880eea5eea007e8baa0f96d90b46a7f5672c5ed6121',
}
RECIPE = Path('recipes-kernel/v4l2loopback/v4l2loopback_0.15.4.bb')
PATCH = RECIPE.parent / 'files/0001-output-completion-queue.patch'
FRESH_PATCH = RECIPE.parent / 'files/0002-fresh-capture-start.patch'
PATCHES = (PATCH, FRESH_PATCH)


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args], text=True, timeout=10).strip()


def sha(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError('expected regular owned source file: ' + str(path))
    return hashlib.sha256(path.read_bytes()).hexdigest()


def validate(source, builder, pins=PINS):
    source, builder = Path(source).resolve(strict=True), Path(builder).resolve(strict=True)
    if git(source, 'rev-parse', 'HEAD') != pins['upstream_revision']:
        raise ValueError('unexpected upstream revision')
    if git(builder, 'rev-parse', 'HEAD') != pins['builder_revision']:
        raise ValueError('unexpected Builder revision')
    # The initial workflow build deliberately removes the upstream snapshot
    # marker, matching the release tarball's canonical MODULE_VERSION.
    if git(source, 'diff', '--name-status', 'HEAD', '--') not in ('', 'D\t.gitversion'):
        raise ValueError('upstream has changes beyond the release version marker')
    if git(builder, 'diff', '--name-status', 'HEAD', '--'):
        raise ValueError('Builder checkout differs from the pinned commit')
    actual = {
        'source_sha256': sha(source / 'v4l2loopback.c'),
        'recipe_sha256': sha(builder / RECIPE),
        'patch_sha256': sha(builder / PATCH),
        'fresh_patch_sha256': sha(builder / FRESH_PATCH),
    }
    for key, value in actual.items():
        if value != pins[key]:
            raise ValueError('unexpected ' + key)
    recipe = (builder / RECIPE).read_text()
    if recipe.count('SRCREV = "' + pins['upstream_revision'] + '"') != 1:
        raise ValueError('recipe does not select the verified upstream source')
    if re.findall(r'file://([\w.-]+\.patch)', recipe) != [patch.name for patch in PATCHES]:
        raise ValueError('recipe does not apply the exact candidate patches in order')
    return {**pins, 'source': str(source), 'builder': str(builder),
            'patches': [str(builder / patch) for patch in PATCHES]}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    parser.add_argument('builder', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    evidence = validate(args.source, args.builder)
    with args.output.open('x') as stream:
        json.dump(evidence, stream, indent=2)
        stream.write('\n')
