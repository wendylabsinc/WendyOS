#!/usr/bin/env python3
"""Bind the kernel comparison to the exact public Builder recipe and patch."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

PINS = {
    'upstream_revision': '0f9ee86760b7f2bea174b7e3e7a1d38845da0ab4',
    'builder_revision': '4828717a29aa3e7d3b183c6683155337957af39f',
    'source_sha256': 'a3e78c9ee061e5ce81b0ea45bb1730ce531b5ece7194f56a47d831ec2d009c2b',
    'patched_source_sha256': '10d2f6b45304ce6018741f35346f96154319aae2706c1112cd436c9b8e388be2',
    'recipe_sha256': '8938e74149f7679777a938e58ed8edc9e65ec8f39755867709ee36b5d74a2ffc',
    'patch_sha256': '57d6b61e2335cc3225dd35c667bc2eba58682ff3e611905ce970144d761493e5',
}
RECIPE = Path('recipes-kernel/v4l2loopback/v4l2loopback_0.15.4.bb')
PATCH = RECIPE.parent / 'files/0001-output-completion-queue.patch'


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
    }
    for key, value in actual.items():
        if value != pins[key]:
            raise ValueError('unexpected ' + key)
    recipe = (builder / RECIPE).read_text()
    if recipe.count('SRCREV = "' + pins['upstream_revision'] + '"') != 1:
        raise ValueError('recipe does not select the verified upstream source')
    if recipe.count('file://' + PATCH.name) != 1:
        raise ValueError('recipe does not apply the exact candidate patch')
    return {**pins, 'source': str(source), 'builder': str(builder), 'patch': str(builder / PATCH)}


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
