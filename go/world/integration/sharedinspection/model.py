"""Shared perception plus the existing signed, simulation-only Atlas task stack."""
from copy import deepcopy
import base64
import ipaddress

from integration.remotemediafusion import libraries

_shared = libraries.load('sharedinspection_shared_model', libraries.HERE.parent / 'sharedperception/model.py')
APP = 'com.wendy.world.sharedinspection'
NON_WORLD_SERVICES = ('scene', 'mission-gateway', 'atlas-executor')
SERVICES = {**_shared.SERVICES, 'a': (*_shared.SERVICES['a'], 'mission-gateway', 'atlas-executor'),
            'b': (*_shared.SERVICES['b'], 'viewer')}
VIEWER_AUDIENCE = 'sharedinspection-native-viewer'
VIEWER_PORT = 8094
VIEWER_MEDIA_SOURCES = (('a', 'camera-a'), ('a', 'people-front'), ('a', 'people-oblique'),
                        ('b', 'camera-b'), ('b', 'audio-1'))
for _name in ('CAPTURE', 'RESULT', 'STREAM', 'ANCHOR', 'CAMERAS', 'LIDARS', 'HAZARD_SENSORS',
              'PEOPLE_CAMERAS', 'SENSORS', 'REMOTE_SENSORS', 'LOCAL_SENSORS', 'PAIR', 'TOOLS',
              'PROJECT', 'MAX_JSON', 'Refused', 'require', 'strict_json', 'json_bytes', 'digest',
              'digest_shape', 'read_regular', 'validate_pair', 'validate_peers', 'mesh_configuration',
              'config_path', 'state_path', 'sample_domain', 'validate_scene_identity'):
    globals()[_name] = getattr(_shared, _name)


def volume_names(run_id, node, service):
    sample_domain(run_id)
    require(node in SERVICES and service in SERVICES[node], 'service belongs to another node')
    kinds = ('config', 'proofs', 'state') if service == 'mission-gateway' else ('config', 'proofs')
    return {kind: 'world-sharedinspection-' + run_id + '-' + node + '-' + service + '-' + kind for kind in kinds}


# Every module here is newly loaded for this fixture. Existing fixture globals
# and their previously sealed stages remain unchanged.
for _module in (_shared, _shared._remote, _shared._remote._base, _shared._people):
    _module.APP, _module.SERVICES, _module.volume_names = APP, SERVICES, volume_names

capture_source = _shared.capture_source
result_source = _shared.result_source
track_source = _shared.track_source
fusion_config = _shared.fusion_config
people_config = _shared.people_config


def viewer_selectors(pair, *, wire=False):
    return [_shared._remote._base._result_selector(pair, wire=wire),
            _shared._people._track_selector(pair, wire=wire),
            *viewer_evidence_selectors(pair, wire=wire)]


def viewer_evidence_selectors(pair, *, wire=False):
    """Media is owned by its capture service, not the derived result publisher."""
    component = 'componentType' if wire else 'component_type'
    return [{'device': pair['nodes'][('a', 'b').index(node)]['public']['principal'],
             'app': APP, 'service': service, component: CAPTURE, 'stream': service}
            for node, service in VIEWER_MEDIA_SOURCES]


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None):
    value = _shared.commission(node, pair, peers, run_id, scene_identity, scene_url, read_token_file)
    # B already owns camera-b/audio-1. Its existing peer evidence adapter also
    # needs the three A-owned capture streams referenced by the native results.
    captures = []
    for source in viewer_evidence_selectors(pair):
        if source['device'] != pair['nodes'][0]['public']['principal']:
            continue
        source = deepcopy(source)
        source.update(tenant=pair['tenant'], site=scene_identity['site'],
                      component=source.pop('component_type'))
        captures.append(source)
    peer = value['replication']['peers'][0]
    peer['export' if node == 'a' else 'import'].extend(captures)
    if node == 'b':
        peer['evidence_path'] = state_path('b') + '/remote-evidence-a'
        value['policy']['grants'].append({'app': APP, 'service': 'viewer',
                                          'subscribe': viewer_selectors(pair)})
    return value


def capture_config(*args, **kwargs):
    value = _shared.capture_config(*args, **kwargs)
    value.update(max_samples=64, poll_interval_ms=1000)
    return value


def people_reader_config(*args, **kwargs):
    value = _shared.people_reader_config(*args, **kwargs)
    value['mission_floor_y'] = .24
    return value


def reader_config(*args, **kwargs):
    value = _shared.reader_config(*args, **kwargs)
    # Enables the existing exact-original mission handoff. Hazard targets keep
    # their original three-dimensional position; this does not flatten them.
    value['mission_floor_y'] = .24
    return value


def viewer_config(pair, run_id, identity, task, public):
    scene = validate_scene_identity(run_id, identity)
    require(isinstance(task, dict) and task.get('node') == 'b' and task.get('app') == APP and
            task.get('service') == 'viewer' and task.get('device') == pair['nodes'][1]['public']['principal'] and
            type(task.get('generation')) is int and 0 < task['generation'] < 1 << 64 and
            isinstance(task.get('socket_directory'), str) and
            task['socket_directory'].startswith(state_path('b') + '/sockets/'), 'observed viewer World task required')
    fields = {'version', 'tenant', 'site', 'audience', 'root_public_key', 'revocations'}
    require(isinstance(public, dict) and set(public) == fields and type(public['version']) is int and
            public['version'] == 1 and public['tenant'] == pair['tenant'] and public['site'] == scene['site'] and
            public['audience'] == VIEWER_AUDIENCE and isinstance(public['root_public_key'], str) and
            isinstance(public['revocations'], dict), 'exact public viewer trust required')
    try:
        root = base64.b64decode(public['root_public_key'], validate=True)
    except (ValueError, TypeError):
        raise ValueError('viewer root encoding differs') from None
    require(len(root) == 32, 'viewer Ed25519 public root required')
    # Both existing sessions use this one mounted socket identity. Only their
    # exact A-owned result selectors differ; mission projection is not requested.
    hazards = reader_config(pair, run_id, scene, task['generation'])
    people = _shared.people_reader_config(pair, run_id, scene, task['generation'])
    for value in (hazards, people):
        value.update(service='viewer', duration_ms=120000, max_ticks=480)
        value.pop('mission_floor_y', None)
    require({k: v for k, v in hazards.items() if k != 'source'} ==
            {k: v for k, v in people.items() if k != 'source'}, 'one viewer identity and clock configuration required')
    return {'version': 1, 'listen': '0.0.0.0:' + str(VIEWER_PORT), 'allow_insecure_simulation': True,
            'audience': VIEWER_AUDIENCE, 'root_public_key': public['root_public_key'],
            'revocations': deepcopy(public['revocations']), 'request_timeout_ms': 5000,
            'evidence_sources': viewer_evidence_selectors(pair),
            'reader': {'version': 1, 'continuous': True, 'people': people, 'hazards': hazards}}


def app_manifest(pair, run_id, node):
    value = _shared.app_manifest(pair, run_id, node)
    if node == 'a':
        for service in NON_WORLD_SERVICES[1:]:
            entitlements = value['services'][service]['entitlements']
            require(not any(item['type'] == 'world' for item in entitlements), 'mission service must use existing HTTP authority')
            entitlements.append({'type': 'network', 'mode': 'bridge'})
    else:
        value['services']['viewer']['entitlements'].extend([
            {'type': 'world', 'subscribe': viewer_selectors(pair, wire=True)},
            {'type': 'network', 'mode': 'bridge'}])
    return value


def mission_input(pair, run_id, identity, presentation, scene_url, gateway_ip, office_sha256, operator_url='', viewer_url=''):
    validate_pair(pair)
    scene = validate_scene_identity(run_id, identity)
    require(str(ipaddress.IPv4Address(gateway_ip)) == gateway_ip, 'observed mission service IPv4 required')
    geometry = presentation.get('geometry', {}) if isinstance(presentation, dict) else {}
    require(geometry.get('run_id') == run_id, 'original shared presentation run differs')
    model_sha = geometry.get('model_sha256')
    require(digest_shape(model_sha) and scene['frame'] == {'id': 'office-floor', 'revision': 'shared-office-' + model_sha[:16]},
            'observed inspection model/frame required')
    require(digest_shape(office_sha256), 'pinned existing office metadata required')
    value = {'version': 1, 'tenant': pair['tenant'], 'device': pair['nodes'][0]['public']['principal'],
             'site': scene['site'], 'run_id': run_id, 'frame': deepcopy(scene['frame']),
             'package_sha256': scene['package_sha256'], 'model_sha256': model_sha,
             'office_scene_sha256': office_sha256, 'scene_url': scene_url,
             'gateway_url': 'http://' + gateway_ip + ':8093'}
    if operator_url:
        value['operator_url'] = operator_url
    if viewer_url:
        value['viewer_url'] = viewer_url
    return value
