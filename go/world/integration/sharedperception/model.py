"""One observed office source, five-source hazards and two-camera people tracks."""
from copy import deepcopy

from integration.remotemediafusion import libraries

_remote = libraries.load('sharedperception_remote_model', libraries.HERE / 'model.py', {'libraries': libraries})
_people = libraries.load('sharedperception_people_model', libraries.HERE.parent / 'people/model.py')
APP = 'com.wendy.world.sharedperception'
CAPTURE, RESULT, STREAM, ANCHOR = _remote.CAPTURE, _remote.RESULT, _remote.STREAM, _remote.ANCHOR
CAMERAS, LIDARS = _remote.CAMERAS, _remote.LIDARS
HAZARD_SENSORS = _remote.SENSORS
PEOPLE_CAMERAS = ('people-front', 'people-oblique')
SENSORS = (*HAZARD_SENSORS, *PEOPLE_CAMERAS)
REMOTE_SENSORS = _remote.REMOTE_SENSORS
LOCAL_SENSORS = tuple(sensor for sensor in SENSORS if sensor not in REMOTE_SENSORS)
SERVICES = {'a': ('scene', *LOCAL_SENSORS, 'fusion', 'people'),
            'b': (*REMOTE_SENSORS, 'reader', 'people-reader')}

for _name in ('PAIR', 'TOOLS', 'PROJECT', 'MAX_JSON', 'Refused', 'require', 'strict_json',
              'json_bytes', 'digest', 'digest_shape', 'read_regular', 'validate_pair',
              'validate_peers', 'mesh_configuration', 'config_path', 'state_path', 'sample_domain'):
    globals()[_name] = getattr(_remote, _name)


def validate_scene_identity(run_id, identity):
    """Retain the actual shared source identity; never relabel an older scene."""
    fields = {'schema_version', 'run_id', 'boot_id', 'package_sha256', 'frame',
              'clock_domain', 'anchor_unix_ns', 'site', 'scenario_version', 'seed'}
    require(isinstance(identity, dict) and set(identity) == fields, 'shared scene identity fields differ')
    require(identity['schema_version'] == 'wendy.sim.clock.v1' and identity['run_id'] == run_id and
            identity['clock_domain'] == sample_domain(run_id), 'shared scene run or clock differs')
    require(type(identity['anchor_unix_ns']) is int and identity['anchor_unix_ns'] == ANCHOR,
            'fixed shared simulation anchor required')
    require(_people._text(identity['boot_id']) and digest_shape(identity['package_sha256']), 'scene boot/package required')
    frame = identity['frame']
    require(isinstance(frame, dict) and set(frame) == {'id', 'revision'} and
            all(_people._text(value, 256) for value in frame.values()), 'observed shared frame required')
    require(identity['site'] == 'studio-01' and identity['scenario_version'] == 'shared-office-perception-v1' and
            type(identity['seed']) is int and identity['seed'] == 7, 'shared scene metadata differs')
    return deepcopy(identity)


def volume_names(run_id, node, service):
    sample_domain(run_id)
    require(node in SERVICES and service in SERVICES[node], 'service belongs to another node')
    return {kind: 'world-sharedperception-' + run_id + '-' + node + '-' + service + '-' + kind
            for kind in ('config', 'proofs')}


# These are isolated module instances. Original standalone fixtures are unchanged.
for _module in (_remote, _remote._base, _people):
    _module.APP, _module.SERVICES = APP, SERVICES
    _module.validate_scene_identity = validate_scene_identity
    _module.volume_names = volume_names
_people.CAMERAS = PEOPLE_CAMERAS

capture_source = _remote.capture_source
result_source = _remote.result_source
track_source = _people.track_source
fusion_config = _remote.fusion_config
people_config = _people.people_config
reader_config = _remote.reader_config


def capture_config(service, run_id, scene_identity, scene_url, read_token_file='/config/scene-read-token'):
    builder = _people.camera_config if service in PEOPLE_CAMERAS else _remote.capture_config
    return builder(service, run_id, scene_identity, scene_url, read_token_file)


def people_reader_config(pair, run_id, scene_identity, expected_generation=0):
    value = _people.reader_config(pair, run_id, scene_identity, expected_generation)
    value['service'] = 'people-reader'
    return value


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None):
    value = _remote.commission(node, pair, peers, run_id, scene_identity, scene_url, read_token_file)
    kwargs = {'scene_url': scene_url, 'read_token_file': read_token_file} if node == 'a' else {}
    people = _people.commission(node, pair, peers, run_id, scene_identity, **kwargs)
    grants = people['policy']['grants']
    if node == 'b':
        grants[0]['service'] = 'people-reader'
    value['policy']['grants'].extend(grants)
    direction = 'export' if node == 'a' else 'import'
    value['replication']['peers'][0][direction].append(track_source(pair, scene_identity))
    return value


def app_manifest(pair, run_id, node):
    value = _remote.app_manifest(pair, run_id, node)
    people = _people.app_manifest(pair, run_id, node)
    for service in (*PEOPLE_CAMERAS, 'people') if node == 'a' else ():
        value['services'][service] = people['services'][service]
    if node == 'b':
        value['services']['people-reader']['entitlements'].append(
            {'type': 'world', 'subscribe': [_people._track_selector(pair, wire=True)]})
    return value
