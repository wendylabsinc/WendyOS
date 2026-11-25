"""Pure configuration for the two-agent, image-derived people demonstration."""
from copy import deepcopy
from pathlib import PurePosixPath
from urllib.parse import urlsplit

from integration.agentpair import model as pair_model

# Reuse the existing disposable pair's public metadata and filesystem layout.
PAIR, TOOLS, PROJECT = pair_model.PAIR, pair_model.TOOLS, pair_model.PROJECT
MAX_JSON = pair_model.MAX_JSON
Refused = pair_model.Refused
require = pair_model.require
strict_json = pair_model.strict_json
json_bytes = pair_model.json_bytes
digest = pair_model.digest
digest_shape = pair_model.digest_shape
read_regular = pair_model.read_regular
validate_pair = pair_model.validate_pair
validate_peers = pair_model.validate_peers
mesh_configuration = pair_model.mesh_configuration
config_path = pair_model.config_path
state_path = pair_model.state_path
sample_domain = pair_model.sample_domain

APP = 'com.wendy.world.people'
CAPTURE = 'wendy.sensor.capture.v1'
RESULT = 'wendy.inference.result.v1'
STREAM = 'tracks'
ANCHOR = 1767225600000000000
SERVICES = {'a': ('scene', 'camera-a', 'camera-b', 'people'), 'b': ('reader',)}
CAMERAS = ('camera-a', 'camera-b')


def _node(node):
    require(node in SERVICES, 'unknown people node')
    return ('a', 'b').index(node)


def _text(value, maximum=128):
    return (isinstance(value, str) and 0 < len(value) <= maximum and
            value == value.strip() and not any(c in value for c in '\x00\r\n'))


def validate_scene_identity(run_id, identity):
    """Copy the observed /v1/identity, without deriving a replacement boot/frame."""
    domain = sample_domain(run_id)
    fields = {'schema_version', 'run_id', 'boot_id', 'package_sha256', 'frame',
              'clock_domain', 'anchor_unix_ns', 'site', 'scenario_version', 'seed'}
    require(isinstance(identity, dict) and set(identity) == fields, 'scene identity fields differ')
    require(identity['schema_version'] == 'wendy.sim.clock.v1' and
            identity['run_id'] == run_id and identity['clock_domain'] == domain,
            'scene run or clock domain differs')
    require(type(identity['anchor_unix_ns']) is int and identity['anchor_unix_ns'] == ANCHOR,
            'fixed people simulation anchor required')
    require(_text(identity['boot_id']) and digest_shape(identity['package_sha256']), 'scene boot and package required')
    frame = identity['frame']
    require(isinstance(frame, dict) and set(frame) == {'id', 'revision'} and
            all(_text(v, 256) for v in frame.values()), 'observed scene frame required')
    require(identity['site'] == 'studio-01' and identity['scenario_version'] == 'people-annex-v1' and
            type(identity['seed']) is int and identity['seed'] == 7, 'people scene metadata differs')
    return deepcopy(identity)


def _source_url(value):
    require(isinstance(value, str), 'observed scene URL required')
    try:
        url = urlsplit(value)
        port = url.port
    except ValueError as error:
        raise Refused('observed scene URL required') from error
    require(url.scheme == 'http' and url.hostname and port is not None and 0 < port < 65536 and
            url.path == '' and not url.query and not url.fragment and
            url.username is None and url.password is None, 'HTTP scene origin required')
    return value


def _path(value):
    require(isinstance(value, str) and value.startswith('/') and value != '/' and
            str(PurePosixPath(value)) == value and '..' not in PurePosixPath(value).parts,
            'absolute config path required')
    return value


def _capture_selector(camera, *, wire=False):
    key = 'componentType' if wire else 'component_type'
    return {'app': APP, 'service': camera, key: CAPTURE, 'stream': camera}


def _track_selector(pair, *, wire=False):
    key = 'componentType' if wire else 'component_type'
    return {'device': pair['nodes'][0]['public']['principal'], 'app': APP,
            'service': 'people', key: RESULT, 'stream': STREAM}


def track_source(pair, identity):
    """The only replicated source is A's generic, typed track result."""
    return {'tenant': pair['tenant'], 'site': identity['site'],
            'device': pair['nodes'][0]['public']['principal'], 'app': APP,
            'service': 'people', 'component': RESULT, 'stream': STREAM}


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None):
    validate_peers(pair, peers)
    index = _node(node)
    scene = validate_scene_identity(run_id, scene_identity)
    public = pair['nodes'][index]['public']
    remote = pair['nodes'][1-index]['public']
    state = state_path(node)
    grants = []
    if node == 'a':
        grants.extend({'app': APP, 'service': camera,
                       'publish': [{'component_type': CAPTURE, 'stream': camera}]} for camera in CAMERAS)
        grants.append({'app': APP, 'service': 'people',
                       'publish': [{'component_type': RESULT, 'stream': STREAM}],
                       'subscribe': [_capture_selector(camera) for camera in CAMERAS]})
    else:
        grants.append({'app': APP, 'service': 'reader', 'subscribe': [_track_selector(pair)]})
    track = track_source(pair, scene)
    value = {'policy': {'version': 1, 'tenant': pair['tenant'], 'site': scene['site'],
                        'device': public['principal'], 'execution_domain': 'simulation',
                        'sample_clock_domain': scene['clock_domain'], 'frames': [scene['frame']],
                        'not_before': public['not_before'], 'not_after': public['not_after'], 'grants': grants},
             'roots_der': [public['root_der']], 'socket_directory': state + '/sockets',
             'journal_path': state + '/journal/journal.db',
             'limits': {'MessagesPerSecond': 200, 'MessageBurst': 400,
                        'BytesPerSecond': 8 << 20, 'ByteBurst': 16 << 20},
             'replication': {'authority_path': state + '/authority.db', 'peers': [{
                 'device': remote['principal'], 'credential_sha256': peers['nodes'][1-index]['leaf_sha256'],
                 'generation': 1, 'remote_path': state + '/remote.db',
                 'import': [track] if node == 'b' else [], 'export': [track] if node == 'a' else []}]}}
    if node == 'a':
        value.update(proof_archive_path=state + '/archive/proofs.db', evidence_directory=state + '/evidence',
                     simulation_clock={'url': _source_url(scene_url), 'read_token_file': _path(read_token_file),
                                       'run_id': run_id, 'boot_id': scene['boot_id'],
                                       'package_sha256': scene['package_sha256'], 'frame': deepcopy(scene['frame']),
                                       'anchor_unix_ns': scene['anchor_unix_ns']})
    else:
        require(scene_url is None and read_token_file is None, 'receiver uses the explicit mirrored clock file')
        value['simulation_clock_file'] = public['simulation_clock_path']
    return value


def camera_config(service, run_id, scene_identity, scene_url, read_token_file='/config/scene-read-token'):
    scene = validate_scene_identity(run_id, scene_identity)
    require(service in CAMERAS, 'known camera required')
    return {'scene_url': _source_url(scene_url), 'read_token_file': _path(read_token_file),
            'run_id': run_id, 'site': scene['site'], 'frame': scene['frame'], 'sensor_id': service,
            'simulation_anchor_unix_ns': scene['anchor_unix_ns'], 'duration_ms': 60000,
            'max_samples': 32, 'poll_interval_ms': 100, 'ttl_ms': 3000, 'request_timeout_ms': 1500}


def _app_config(pair, node, service, run_id, identity, expected_generation):
    validate_pair(pair)
    scene = validate_scene_identity(run_id, identity)
    require(type(expected_generation) is int and 0 <= expected_generation < 1 << 64, 'observed generation required')
    public = pair['nodes'][_node(node)]['public']
    value = {'version': 1, 'tenant': pair['tenant'], 'site': scene['site'], 'device': public['principal'],
             'app': APP, 'service': service, 'roots_der': [public['root_der']], 'run_id': run_id,
             'frame': scene['frame'], 'duration_ms': 60000, 'request_timeout_ms': 3000,
             'poll_interval_ms': 250, 'clock_max_width_ms': 5000}
    if expected_generation:
        value['expected_generation'] = expected_generation
    return value, scene


def people_config(pair, run_id, scene_identity, model, expected_generation=0):
    value, scene = _app_config(pair, 'a', 'people', run_id, scene_identity, expected_generation)
    require(isinstance(model, dict) and set(model) == {'model_id', 'model_version', 'model_sha256'} and
            _text(model['model_id'], 256) and _text(model['model_version'], 256) and digest_shape(model['model_sha256']),
            'exact pose model identity required')
    value.update(scene_boot_id=scene['boot_id'], package_sha256=scene['package_sha256'],
                 simulation_anchor_unix_ns=scene['anchor_unix_ns'], scenario_version=scene['scenario_version'],
                 seed=scene['seed'], sources=[{'sensor_id': c, 'app': APP, 'service': c} for c in CAMERAS],
                 result_stream=STREAM, max_ticks=120, retain_for_ms=60000, floor_y=0.0, ankle_height_m=.08, **model)
    return value


def reader_config(pair, run_id, scene_identity, expected_generation=0):
    value, _ = _app_config(pair, 'b', 'reader', run_id, scene_identity, expected_generation)
    value.update(source={key: item for key, item in _track_selector(pair).items() if key != 'component_type'}, max_ticks=240)
    return value


def volume_names(run_id, node, service):
    sample_domain(run_id)
    _node(node)
    require(service in SERVICES[node], 'service belongs to another node')
    kinds = ('config', 'proofs')
    return {kind: 'world-people-' + run_id + '-' + node + '-' + service + '-' + kind for kind in kinds}


def app_manifest(pair, run_id, node):
    validate_pair(pair)
    _node(node)
    services = {}
    for service in SERVICES[node]:
        entitlements = [{'type': 'persist', 'name': name, 'path': '/' + kind}
                        for kind, name in volume_names(run_id, node, service).items()]
        if service in CAMERAS:
            entitlements.append({'type': 'world', 'publish': [{'componentType': CAPTURE, 'stream': service}]})
        elif service == 'people':
            entitlements.append({'type': 'world', 'publish': [{'componentType': RESULT, 'stream': STREAM}],
                                 'subscribe': [_capture_selector(c, wire=True) for c in CAMERAS]})
        elif service == 'reader':
            entitlements.append({'type': 'world', 'subscribe': [_track_selector(pair, wire=True)]})
        services[service] = {'context': 'services/' + service, 'entitlements': entitlements}
    return {'appId': APP, 'version': '1.0.0', 'platform': 'linux', 'isolation': 'isolated', 'services': services}
