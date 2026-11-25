"""Bounded public inputs and commissioning for the disposable two-agent run."""
import base64
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import stat
import uuid

APP = 'com.wendy.world.agentpair'
SERVICE = 'probe'
COMPONENT = 'wendy.sensor.scalar.v1'
STREAM = 'gas'
PAIR = '/var/lib/agentpair/enrollment'
TOOLS = '/var/lib/agentpair/tools'
PROJECT = '/var/lib/agentpair/project'
PORT = 43020
FRAME = {'id': 'office-floor', 'revision': 'agentpair-1'}
SAMPLE_START = '2026-01-01T00:00:00Z'
TOOLS_REQUIRED = ('agent', 'cli', 'fixture', 'pair-fixture', 'initializer', 'probe')
TOOLS_OPTIONAL = ('proof',)
MAX_JSON = 256 << 10


class Refused(ValueError):
    pass


def require(ok, reason):
    if not ok:
        raise Refused(reason)


def json_bytes(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False) + '\n').encode()


def strict_json(raw, limit=MAX_JSON):
    require(isinstance(raw, bytes) and 0 < len(raw) <= limit, 'bounded JSON required')
    def pairs(items):
        value = {}
        for key, item in items:
            require(key not in value, 'duplicate JSON key')
            value[key] = item
        return value
    def invalid(_):
        raise Refused('nonfinite JSON number')
    try:
        return json.loads(raw, object_pairs_hook=pairs, parse_constant=invalid)
    except (UnicodeError, ValueError, RecursionError) as error:
        raise Refused('invalid bounded JSON') from error


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def digest_shape(value):
    return isinstance(value, str) and re.fullmatch('[a-f0-9]{64}', value) is not None


def canonical_uuid(value):
    try:
        return str(uuid.UUID(value)) == value
    except (ValueError, AttributeError, TypeError):
        return False


def state_path(node):
    require(node in ('a', 'b'), 'unknown node')
    return '/var/lib/agentpair/state-' + node


def config_path(node):
    require(node in ('a', 'b'), 'unknown node')
    return PAIR + '/' + node


def sample_domain(run_id):
    # serviceorigin binds original simulation provenance to this exact form.
    # The run identifier itself already distinguishes each disposable pair.
    require(isinstance(run_id, str) and re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', run_id), 'invalid simulation run identifier')
    return 'simulation:' + run_id


def read_regular(path, maximum):
    path = Path(path)
    require(path.is_absolute() and '..' not in path.parts, 'clean absolute input required')
    directory = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory); directory = child
            info = os.fstat(directory)
            require(info.st_uid in (0, os.geteuid()) and (not info.st_mode & 0o022 or info.st_uid == 0 and info.st_mode & stat.S_ISVTX), 'unprotected input ancestor')
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    finally:
        os.close(directory)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid == os.geteuid() and not info.st_mode & 0o022 and 0 < info.st_size <= maximum, 'invalid regular input')
        raw = stream.read(maximum + 1)
        after = os.fstat(stream.fileno())
    require(len(raw) == info.st_size and (info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns) == (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns), 'input changed')
    return raw


def validate_inputs(raw):
    value = strict_json(raw)
    require(isinstance(value, dict) and set(value) == {'version', 'run_id', 'runtime_image', 'binaries'}, 'unexpected input fields')
    require(type(value['version']) is int and value['version'] == 1, 'input version')
    require(isinstance(value['run_id'], str) and re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', value['run_id']), 'invalid run identifier')
    require(isinstance(value['runtime_image'], str) and re.fullmatch('sha256:[a-f0-9]{64}', value['runtime_image']), 'immutable local runtime image required')
    require(isinstance(value['binaries'], dict) and
            set(TOOLS_REQUIRED) <= set(value['binaries']) <= set(TOOLS_REQUIRED + TOOLS_OPTIONAL),
            'required binaries and only supported optional tools allowed')
    for name, row in value['binaries'].items():
        require(isinstance(row, dict) and set(row) == {'path', 'sha256'}, 'invalid binary pin')
        require(isinstance(row['path'], str) and Path(row['path']).is_absolute() and digest_shape(row['sha256']), 'absolute binary path and digest required')
        body = read_regular(row['path'], 96 << 20)
        require(digest(body) == row['sha256'], 'binary digest differs')
        require(body[:6] == b'\x7fELF\x02\x01' and len(body) >= 64 and int.from_bytes(body[18:20], 'little') == 183, 'Linux arm64 ELF required')
    return value


def validate_pair(pair):
    require(isinstance(pair, dict) and pair.get('kind') == 'UNTRUSTED-PRIVATE-AGENT-PAIR-FIXTURE-v1', 'paired fixture marker required')
    require(canonical_uuid(pair.get('pair_id')) and canonical_uuid(pair.get('tenant')), 'canonical pair identity required')
    require(digest_shape(pair.get('root_sha256')) and digest_shape(pair.get('issuer_sha256')), 'trust digests required')
    require(isinstance(pair.get('nodes'), list) and len(pair['nodes']) == 2, 'two pair nodes required')
    identities = set()
    for index, name in enumerate(('a', 'b')):
        node = pair['nodes'][index]
        require(node.get('directory') == config_path(name) and node.get('state_directory') == state_path(name), 'pair paths differ')
        public = node.get('public', {})
        require(public.get('kind') == 'UNTRUSTED-PRIVATE-AGENT-VM-FIXTURE-v1' and public.get('pair_id') == pair['pair_id'], 'pair node marker differs')
        device = public.get('device_id')
        require(canonical_uuid(device) and device not in identities, 'distinct device IDs required')
        identities.add(device)
        require(public.get('tenant') == pair['tenant'] and public.get('principal') == 'spiffe://wendy.sh/tenant/' + pair['tenant'] + '/device/' + device, 'device principal differs')
        require(type(public.get('leaf_serial')) is int and public['leaf_serial'] == index + 3, 'distinct leaf serial required')
        require(public.get('config_path') == config_path(name) + '/world.json' and public.get('simulation_clock_path') == config_path(name) + '/simclock.json', 'node config paths differ')
        for prefix in ('root', 'issuer'):
            try:
                certificate = base64.b64decode(public[prefix + '_der'], validate=True)
            except (ValueError, KeyError, TypeError) as error:
                raise Refused('invalid public certificate encoding') from error
            require(0 < len(certificate) <= 32 << 10 and digest(certificate) == pair[prefix + '_sha256'] == public.get(prefix + '_sha256'), 'public trust differs')
        require(digest_shape(node.get('public_sha256')), 'public node digest required')
    return pair


def validate_peers(pair, peers):
    validate_pair(pair)
    require(isinstance(peers, dict) and peers.get('pair_id') == pair['pair_id'] and peers.get('tenant') == pair['tenant'] and peers.get('root_sha256') == pair['root_sha256'], 'peer receipt belongs to another pair')
    require(peers.get('kind') == pair['kind'] and peers.get('authority_time') == 'fixture-host-wall-time-only' and peers.get('issuer_sha256') == pair['issuer_sha256'], 'public fingerprint receipt required')
    require(isinstance(peers.get('nodes'), list) and len(peers['nodes']) == 2, 'two issued peers required')
    seen_leaf, seen_key = set(), set()
    for index, item in enumerate(peers['nodes']):
        public = pair['nodes'][index]['public']
        require(item.get('device_id') == public['device_id'] and item.get('principal') == public['principal'] and item.get('leaf_serial') == public['leaf_serial'], 'issued peer identity differs')
        for field in ('leaf_sha256', 'device_spki_sha256', 'issued_receipt_sha256'):
            require(digest_shape(item.get(field)), 'issued fingerprint missing')
        require(item['leaf_sha256'] not in seen_leaf and item['device_spki_sha256'] not in seen_key, 'distinct issued device certificates required')
        seen_leaf.add(item['leaf_sha256']); seen_key.add(item['device_spki_sha256'])
    return peers


def source(pair, index):
    return {'tenant': pair['tenant'], 'site': 'office', 'device': pair['nodes'][index]['public']['principal'],
            'app': APP, 'service': SERVICE, 'component': COMPONENT, 'stream': STREAM}


def commission(pair, peers, node, run_id):
    validate_peers(pair, peers)
    require(node in ('a', 'b') and re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', run_id), 'node/run required')
    index = ('a', 'b').index(node)
    public = pair['nodes'][index]['public']; remote = source(pair, 1-index); local = source(pair, index)
    subscription = {'app': APP, 'service': SERVICE, 'component_type': COMPONENT, 'stream': STREAM, 'device': remote['device']}
    domain = sample_domain(run_id)
    policy = {'version': 1, 'tenant': pair['tenant'], 'site': 'office', 'device': public['principal'],
              'execution_domain': 'simulation', 'sample_clock_domain': domain, 'frames': [dict(FRAME)],
              'not_before': public['not_before'], 'not_after': public['not_after'],
              'grants': [{'app': APP, 'service': SERVICE, 'publish': [{'component_type': COMPONENT, 'stream': STREAM}], 'subscribe': [subscription]}]}
    state = state_path(node)
    return {'policy': policy, 'roots_der': [public['root_der']], 'socket_directory': state + '/sockets',
            'journal_path': state + '/journal/journal.db', 'simulation_clock_file': public['simulation_clock_path'],
            'replication': {'authority_path': state + '/authority.db', 'peers': [{
                'device': remote['device'], 'credential_sha256': peers['nodes'][1-index]['leaf_sha256'], 'generation': 1,
                'remote_path': state + '/remote.db', 'import': [remote], 'export': [local]}]}}


def mesh_configuration(pair, node, addresses):
    validate_pair(pair)
    require(set(addresses) == {'a', 'b'} and addresses['a'] != addresses['b'], 'two observed node addresses required')
    for address in addresses.values():
        try:
            parsed = ipaddress.IPv4Address(address)
        except ipaddress.AddressValueError as error:
            raise Refused('private numeric node address required') from error
        require(parsed.is_private and not parsed.is_loopback and not parsed.is_link_local and not parsed.is_unspecified, 'private numeric node address required')
    index = ('a', 'b').index(node); other = ('a', 'b')[1-index]
    return {'listen': addresses[node] + ':' + str(PORT), 'peers': [{'device': pair['nodes'][1-index]['public']['device_id'], 'address': addresses[other] + ':' + str(PORT)}]}


def volume_names(run_id, node, generation):
    require(re.fullmatch('[a-z0-9][a-z0-9-]{0,39}', run_id) and node in ('a', 'b') and type(generation) is int and 1 <= generation <= 8, 'invalid volume scope')
    return {kind: 'world-pair-' + run_id + '-' + node + '-' + str(generation) + '-' + kind for kind in ('config', 'control', 'proofs')}


def app_manifest(pair, run_id, node, generation=1):
    validate_pair(pair)
    index = ('a', 'b').index(node); peer = pair['nodes'][1-index]['public']['principal']
    names = volume_names(run_id, node, generation)
    entitlements = [{'type': 'persist', 'name': names[kind], 'path': '/' + kind} for kind in names]
    entitlements.append({'type': 'world', 'publish': [{'componentType': COMPONENT, 'stream': STREAM}],
                         'subscribe': [{'app': APP, 'service': SERVICE, 'componentType': COMPONENT, 'stream': STREAM, 'device': peer}]})
    return {'appId': APP, 'version': '1.0.0', 'platform': 'linux', 'services': {SERVICE: {'context': '.', 'entitlements': entitlements}}}


def probe_config(pair, run_id, node):
    validate_pair(pair)
    index = ('a', 'b').index(node)
    return {'version': 1, 'tenant': pair['tenant'], 'site': 'office', 'device': pair['nodes'][index]['public']['principal'],
            'peer_device': pair['nodes'][1-index]['public']['principal'], 'run_id': run_id, 'scenario_version': 'agentpair-1',
            'seed': 1, 'frame': dict(FRAME), 'sample_clock_domain': sample_domain(run_id),
            'duration_ms': 600000, 'max_commands': 32, 'request_timeout_ms': 1000, 'clock_max_width_ms': 5000, 'poll_interval_ms': 100}
