"""One scene, B-owned gas, A-owned fusion, and an ordinary hazard reader on B."""
if __package__:
    from . import libraries
else:
    import libraries

_base = libraries.load('distributedfusion_hazard_model', libraries.HERE.parent / 'hazard/model.py')

APP = 'com.wendy.world.distributedfusion'
CAPTURE, RESULT, STREAM, ANCHOR = _base.CAPTURE, _base.RESULT, _base.STREAM, _base.ANCHOR
CAMERAS, LIDARS, SENSORS = _base.CAMERAS, _base.LIDARS, _base.SENSORS
LOCAL_SENSORS = tuple(s for s in SENSORS if s != 'gas-1')
SERVICES = {'a': ('scene', *LOCAL_SENSORS, 'fusion'), 'b': ('gas-1', 'reader')}
_base.APP, _base.SERVICES = APP, SERVICES

for _name in ('PAIR', 'TOOLS', 'PROJECT', 'MAX_JSON', 'Refused', 'require', 'strict_json',
              'json_bytes', 'digest', 'digest_shape', 'read_regular', 'validate_pair',
              'validate_peers', 'mesh_configuration', 'config_path', 'state_path',
              'sample_domain', 'validate_scene_identity', 'capture_config', 'reader_config',
              'result_source'):
    globals()[_name] = getattr(_base, _name)


def gas_source(pair, scene):
    return {'tenant': pair['tenant'], 'site': scene['site'],
            'device': pair['nodes'][1]['public']['principal'], 'app': APP,
            'service': 'gas-1', 'component': CAPTURE, 'stream': 'gas-1'}


def _qualify_gas(selectors, pair):
    for source in selectors:
        if source['service'] == 'gas-1':
            source['device'] = pair['nodes'][1]['public']['principal']


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None):
    # The existing loader creates CaptureSource only for an HTTP clock binding.
    # Both agents therefore read the same source; B never manufactures a clock.
    scene = validate_scene_identity(run_id, scene_identity)
    url, token = _base._source_url(scene_url), _base._path(read_token_file)
    kwargs = {'scene_url': url, 'read_token_file': token} if node == 'a' else {}
    value = _base.commission(node, pair, peers, run_id, scene, **kwargs)
    grants = value['policy']['grants']
    if node == 'a':
        value['policy']['grants'] = [g for g in grants if g['service'] != 'gas-1']
        _qualify_gas(next(g for g in grants if g['service'] == 'fusion')['subscribe'], pair)
    else:
        grants.insert(0, {'app': APP, 'service': 'gas-1',
                         'publish': [{'component_type': CAPTURE, 'stream': 'gas-1'}]})
        del value['simulation_clock_file']
        value.update(proof_archive_path=state_path(node) + '/archive/proofs.db',
                     simulation_clock={'url': url, 'read_token_file': token, 'run_id': run_id,
                         'boot_id': scene['boot_id'], 'package_sha256': scene['package_sha256'],
                         'frame': scene['frame'], 'anchor_unix_ns': scene['anchor_unix_ns']})
    peer = value['replication']['peers'][0]
    if node == 'a':
        peer['import'] = [gas_source(pair, scene)]
    else:
        peer['export'] = [gas_source(pair, scene)]
    return value


def fusion_config(pair, run_id, scene_identity, expected_generation=0):
    value = _base.fusion_config(pair, run_id, scene_identity, expected_generation)
    _qualify_gas(value['sources'], pair)
    return value


def volume_names(run_id, node, service):
    sample_domain(run_id)
    require(node in SERVICES and service in SERVICES[node], 'service belongs to another node')
    return {kind: 'world-distributedfusion-' + run_id + '-' + node + '-' + service + '-' + kind
            for kind in ('config', 'proofs')}


_base.volume_names = volume_names


def app_manifest(pair, run_id, node):
    value = _base.app_manifest(pair, run_id, node)
    if node == 'a':
        world = next(e for e in value['services']['fusion']['entitlements'] if e['type'] == 'world')
        _qualify_gas(world['subscribe'], pair)
    else:
        # The gas source is outside B's per-app CNI subnet. Explicit bridge
        # networking supplies normal NAT egress while retaining its namespace.
        value['services']['gas-1']['entitlements'].append({'type': 'network', 'mode': 'bridge'})
    return value
