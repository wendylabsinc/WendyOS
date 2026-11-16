#!/usr/bin/env python3
"""Verify every release asset's build provenance, then pin its tag to that build.

Called by build.yml with FILES, TAG, GITHUB_SHA, GITHUB_REPOSITORY and GH_TOKEN.
A failed check exits nonzero before the release action. Never moves an existing tag.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import quote


def gh(*args):
    return subprocess.run(['gh', *args], check=True, capture_output=True, text=True, timeout=120).stdout


def tag_commit(repository, tag):
    value = json.loads(gh('api', f'repos/{repository}/git/ref/tags/{quote(tag, safe="")}'))
    seen = set()
    for _ in range(8):
        obj = value['object']
        digest = obj['sha']
        if not isinstance(digest, str) or not re.fullmatch(r'[0-9a-f]{40}', digest):
            raise ValueError('tag returned an invalid object digest')
        if obj['type'] == 'commit':
            return digest
        if obj['type'] != 'tag' or digest in seen:
            raise ValueError('release tag does not resolve to a commit')
        seen.add(digest)
        value = json.loads(gh('api', f'repos/{repository}/git/tags/{digest}'))
    raise ValueError('release tag nesting exceeds eight objects')


def verify_release():
    repository, built, tag = (os.environ[k] for k in ('GITHUB_REPOSITORY', 'GITHUB_SHA', 'TAG'))
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
        raise ValueError('GITHUB_REPOSITORY must identify one repository')
    if not re.fullmatch(r'[0-9a-f]{40}', built) or not tag or any(ord(c) < 32 for c in tag):
        raise ValueError('invalid built commit or release tag')
    files = os.environ.get('FILES', '').splitlines()
    if not files or len(set(files)) != len(files):
        raise ValueError('release requires a nonempty, unique asset inventory')
    for name in files:
        path = Path(name)
        if not path.is_file() or path.is_symlink() or not path.stat().st_size:
            raise ValueError(f'release asset is missing, empty or a symlink: {name}')
    for name in files:
        # gh checks the actual artifact digest against the signed subject. The
        # source constraint comes from the attestation certificate, not a filename
        # or a version string supplied by the build. Do not replace it with a
        # comparison of unverified JSON or generate new provenance in this job.
        gh('attestation', 'verify', name, '--repo', repository,
           '--signer-workflow', f'{repository}/.github/workflows/build.yml',
           '--source-digest', built)
        print(f'Verified {name} from {built}', flush=True)
    try:
        actual = tag_commit(repository, tag)
    except subprocess.CalledProcessError:
        # A missing ref is the ordinary initial publication. Other API failures
        # also reach this attempt, but cannot publish unless the final ref read
        # independently proves the exact built commit. No update/force API is used.
        try:
            gh('api', f'repos/{repository}/git/refs', '-f', f'ref=refs/tags/{tag}', '-f', f'sha={built}')
        except subprocess.CalledProcessError:
            # Another retry may have won the create race. Only the same commit
            # can satisfy the read below; permissions/network failures stay fatal.
            pass
        actual = tag_commit(repository, tag)
    if actual != built:
        raise ValueError(f'tag {tag} points at {actual}, but artifacts were built at {built}; refusing publication')
    print(f'Tag {tag} and all {len(files)} release assets agree on {built}', flush=True)


if __name__ == '__main__':
    try:
        verify_release()
    except (KeyError, ValueError, OSError, subprocess.SubprocessError) as error:
        print(f'::error::Release verification failed: {error}', file=sys.stderr)
        sys.exit(1)
