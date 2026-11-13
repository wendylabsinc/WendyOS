#!/usr/bin/env python3
"""Compare documented configuration with hash-bound published rootfs contents.

SPDX source/recipe nodes are not installed files. Only the unique image SBOM's
rootfs contains relation qualifies. This proves packaged configuration, not boot
ordering, successful OTA, hardware behavior or persistence across a reboot.
"""
import argparse
import configparser
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
PINS = ROOT / '.github/scripts/fixtures/docs-capabilities/pins.json'
DOC = ROOT / 'go/internal/cli/assets/docs/wendyos/services/logging.md'
PREFIX = 'https://github.com/wendylabsinc/WendyOS-Builder/releases/download/'
RAW = 'https://raw.githubusercontent.com/wendylabsinc/WendyOS-Builder/'


def require(value, message):
    if not value:
        raise RuntimeError(message)


def sha(path):
    result = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            result.update(chunk)
    return result.hexdigest()


def installed(document, expected_name):
    require(document.get('@context') == 'https://spdx.org/rdf/3.0.1/spdx-context.jsonld', 'unexpected SPDX context')
    graph = document['@graph']
    identifiers = [item['spdxId'] for item in graph if 'spdxId' in item]
    require(len(identifiers) == len(set(identifiers)), 'duplicate SPDX identity')
    index = {item['spdxId']: item for item in graph if 'spdxId' in item}
    sboms = [item for item in graph if item['type'] == 'software_Sbom']
    require(len(sboms) == 1 and sboms[0]['name'] == expected_name, 'wrong or ambiguous image SBOM')
    roots = [key for key in sboms[0]['rootElement'] if '/rootfs/' in key]
    require(len(roots) == 1 and index[roots[0]]['type'] == 'software_Package', 'missing image rootfs')
    edges = [item for item in graph if item.get('from') == roots[0] and item.get('relationshipType') == 'contains']
    require(len(edges) == 1 and edges[0]['to'], 'empty or ambiguous rootfs membership')
    files = {}
    for key in edges[0]['to']:
        item = index[key]
        require(item['type'] == 'software_File', 'unexpected rootfs member type')
        name = item['name']
        require(name not in files, 'duplicate installed filename: ' + name)
        hashes = [x['hashValue'] for x in item.get('verifiedUsing', []) if x.get('algorithm') == 'sha256']
        require(len(hashes) == 1 and re.fullmatch('[0-9a-f]{64}', hashes[0]), 'missing installed file digest: ' + name)
        files[name] = hashes[0]
    return files


def section(text, name):
    parser = configparser.ConfigParser(interpolation=None, strict=True)
    parser.optionxform = str
    parser.read_string(text)
    require(name in parser, 'missing configuration section ' + name)
    return dict(parser[name])


def check_configuration(documentation, journal, mount):
    blocks = re.findall(r'```ini\n(.*?)\n```', documentation, re.S)
    for name, source in [('Journal', journal), ('Mount', mount)]:
        matches = [block for block in blocks if '[' + name + ']' in block]
        require(len(matches) == 1, 'expected one documented ' + name + ' configuration')
        actual, expected = section(matches[0], name), section(source, name)
        require(actual == expected, f'DOC_PACKAGED_{name.upper()}: documented {actual!r}, installed {expected!r}')


def acquire(directory, key, url, digest, size=None, offline=False):
    path = directory / key
    if not offline:
        require(url.startswith(PREFIX) or url.startswith(RAW), 'unapproved artifact origin')
        subprocess.run(['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https',
                        '--proto-redir', '=https', '--max-time', '45', '--max-filesize', '45000000',
                        '--output', str(path), url], timeout=55, check=True)
    require(path.is_file() and (size is None or path.stat().st_size == size), 'artifact missing/wrong size: ' + key)
    require(sha(path) == digest, 'artifact digest mismatch: ' + key)
    return path


def verify(directory, *, offline=False):
    pins = json.loads(PINS.read_text())
    sources = {}
    for key, pin in pins['sources'].items():
        path = acquire(directory, key + '.source', RAW + pins['builder_revision'] + '/' + pin['path'], pin['sha256'], offline=offline)
        sources[key] = path.read_text()
    results = {}
    for key, pin in pins['images'].items():
        path = acquire(directory, key + '.spdx.json', PREFIX + pins['release'] + '/' + pin['asset'], pin['sha256'], pin['size'], offline)
        files = installed(json.loads(path.read_text()), pin['asset'].removesuffix('.spdx.json'))
        required = {'usr/lib/systemd/system/wendyos-data.service': 'data',
                    'etc/wendyos-update/config.json': pin['config_source']}
        journal = 'usr/lib/systemd/journald.conf.d/10-wendyos-persistent.conf'
        mount = 'usr/lib/systemd/system/var-log.mount'
        require('usr/bin/wendyos-update' in files, 'missing installed OTA engine')
        require(not any('/mender' in name or name.startswith('etc/mender') for name in files), 'Mender unexpectedly installed')
        require((journal in files) == pin['persistent_journal'] and (mount in files) == pin['persistent_journal'], 'journal policy differs from qualified image')
        if pin['persistent_journal']:
            required.update({journal: 'journal', mount: 'mount'})
        for name, source in required.items():
            require(files.get(name) == pins['sources'][source]['sha256'], 'PACKAGED_CONFIG_HASH: ' + name)
        require(json.loads(sources[pin['config_source']])['connector'] == pin['connector'], 'wrong packaged OTA connector')
        results[key] = {'installed_files': len(files), 'connector': pin['connector'],
                        'persistent_journal_configuration': pin['persistent_journal'],
                        'source_bound_files': required, 'spdx_sha256': pin['sha256']}
    check_configuration(DOC.read_text(), sources['journal'], sources['mount'])
    return {'status': 'passed', 'release': pins['release'], 'builder_revision': pins['builder_revision'],
            'images': results, 'documentation_sha256': sha(DOC),
            'scope': 'Published rootfs membership and source-identical packaged configuration; no boot/OTA/reboot acceptance'}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--offline-dir', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='wendy-docs-sbom-') as temporary:
        result = verify(args.offline_dir or Path(temporary), offline=args.offline_dir is not None)
    args.output.write_text(json.dumps(result, indent=2) + '\n')
    print('DOCS_BUILT_CAPABILITIES_VERIFIED')
