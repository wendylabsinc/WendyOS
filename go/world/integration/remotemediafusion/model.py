"""B-owned camera/audio observations, A fusion and an ordinary result reader on B."""
if __package__:
    from . import libraries
else:
    import libraries

_base = libraries.load('remotemediafusion_hazard_model', libraries.HERE.parent / 'hazard/model.py')
APP = 'com.wendy.world.remotemediafusion'
CAPTURE, RESULT, STREAM, ANCHOR = _base.CAPTURE, _base.RESULT, _base.STREAM, _base.ANCHOR
CAMERAS, LIDARS, SENSORS = _base.CAMERAS, _base.LIDARS, _base.SENSORS
REMOTE_SENSORS = ('camera-b', 'audio-1')
LOCAL_SENSORS = tuple(sensor for sensor in SENSORS if sensor not in REMOTE_SENSORS)
SERVICES = {'a': ('scene', *LOCAL_SENSORS, 'fusion'), 'b': (*REMOTE_SENSORS, 'reader')}
_base.APP, _base.SERVICES = APP, SERVICES

for _name in ('PAIR', 'TOOLS', 'PROJECT', 'MAX_JSON', 'Refused', 'require', 'strict_json',
              'json_bytes', 'digest', 'digest_shape', 'read_regular', 'validate_pair',
              'validate_peers', 'mesh_configuration', 'config_path', 'state_path',
              'sample_domain', 'validate_scene_identity', 'capture_config', 'reader_config',
              'result_source'):
    globals()[_name] = getattr(_base, _name)


def capture_source(pair, scene, sensor):
    require(sensor in REMOTE_SENSORS, 'one of the two commissioned remote media sources required')
    return {'tenant': pair['tenant'], 'site': scene['site'],
            'device': pair['nodes'][1]['public']['principal'], 'app': APP,
            'service': sensor, 'component': CAPTURE, 'stream': sensor}


def _qualify_remote(selectors, pair):
    for source in selectors:
        if source['service'] in REMOTE_SENSORS:
            source['device'] = pair['nodes'][1]['public']['principal']


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None):
    scene = validate_scene_identity(run_id, scene_identity)
    url, token = _base._source_url(scene_url), _base._path(read_token_file)
    kwargs = {'scene_url': url, 'read_token_file': token} if node == 'a' else {}
    value = _base.commission(node, pair, peers, run_id, scene, **kwargs)
    grants = value['policy']['grants']
    if node == 'a':
        value['policy']['grants'] = [grant for grant in grants if grant['service'] not in REMOTE_SENSORS]
        _qualify_remote(next(grant for grant in grants if grant['service'] == 'fusion')['subscribe'], pair)
    else:
        value['policy']['grants'] = [
            {'app': APP, 'service': sensor, 'publish': [{'component_type': CAPTURE, 'stream': sensor}]}
            for sensor in REMOTE_SENSORS] + grants
        del value['simulation_clock_file']
        value.update(proof_archive_path=state_path(node) + '/archive/proofs.db',
                     evidence_directory=state_path(node) + '/evidence',
                     simulation_clock={'url': url, 'read_token_file': token, 'run_id': run_id,
                         'boot_id': scene['boot_id'], 'package_sha256': scene['package_sha256'],
                         'frame': scene['frame'], 'anchor_unix_ns': scene['anchor_unix_ns']})
    peer = value['replication']['peers'][0]
    sources = [capture_source(pair, scene, sensor) for sensor in REMOTE_SENSORS]
    if node == 'a':
        peer.update({'import': sources, 'evidence_path': state_path(node) + '/remote-evidence-b'})
    else:
        peer['export'] = sources
    return value


def fusion_config(pair, run_id, scene_identity, expected_generation=0):
    value = _base.fusion_config(pair, run_id, scene_identity, expected_generation)
    _qualify_remote(value['sources'], pair)
    return value


def volume_names(run_id, node, service):
    sample_domain(run_id)
    require(node in SERVICES and service in SERVICES[node], 'service belongs to another node')
    return {kind: 'world-remotemediafusion-' + run_id + '-' + node + '-' + service + '-' + kind
            for kind in ('config', 'proofs')}


_base.volume_names = volume_names


def app_manifest(pair, run_id, node):
    value = _base.app_manifest(pair, run_id, node)
    if node == 'a':
        world = next(entry for entry in value['services']['fusion']['entitlements'] if entry['type'] == 'world')
        _qualify_remote(world['subscribe'], pair)
    else:
        for sensor in REMOTE_SENSORS:
            value['services'][sensor]['entitlements'].append({'type': 'network', 'mode': 'bridge'})
    return value
