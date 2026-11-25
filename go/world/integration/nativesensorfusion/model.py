"""Native camera B ingestion; ordinary local fusion and exact remote result reader."""
if __package__:
    from . import libraries
else:
    import libraries

_base = libraries.load('nativesensorfusion_hazard_model', libraries.HERE.parent / 'hazard/model.py')
APP = 'com.wendy.world.nativesensorfusion'
CAPTURE, RESULT, STREAM, ANCHOR = _base.CAPTURE, _base.RESULT, _base.STREAM, _base.ANCHOR
CAMERAS, LIDARS, SENSORS = _base.CAMERAS, _base.LIDARS, _base.SENSORS
NATIVE_CAMERA, CHANNEL = 'camera-b', 4
HTTP_SENSORS = tuple(s for s in SENSORS if s != NATIVE_CAMERA)
SERVICES = {'a': ('scene', *SENSORS, 'fusion'), 'b': ('reader',)}
_base.APP, _base.SERVICES = APP, SERVICES
for _name in ('PAIR', 'TOOLS', 'PROJECT', 'MAX_JSON', 'Refused', 'require', 'strict_json',
              'json_bytes', 'digest', 'digest_shape', 'read_regular', 'validate_pair',
              'validate_peers', 'mesh_configuration', 'config_path', 'state_path',
              'sample_domain', 'validate_scene_identity', 'fusion_config', 'reader_config',
              'result_source'):
    globals()[_name] = getattr(_base, _name)


def native_source_id(principal):
    require(isinstance(principal, str) and principal.startswith('spiffe://wendy.sh/tenant/') and
            '/device/' in principal and len(principal) <= 220 and
            all(not c.isspace() for c in principal), 'exact enrolled native source principal required')
    return 'sensorlink:' + principal + ':' + str(CHANNEL)


def commission(node, pair, peers, run_id, scene_identity, scene_url=None, read_token_file=None,
               *, source_id=None):
    value = _base.commission(node, pair, peers, run_id, scene_identity, scene_url, read_token_file)
    if node == 'a':
        if source_id is None:
            source_id = native_source_id(pair['nodes'][0]['public']['principal'])
        require(source_id == native_source_id(pair['nodes'][0]['public']['principal']),
                'virtual native source must use the exact enrolled A participant')
        value['native_cameras'] = [{'app': APP, 'service': NATIVE_CAMERA, 'component_type': CAPTURE,
                                    'stream': NATIVE_CAMERA, 'source_id': source_id}]
    else:
        require(source_id is None, 'reader has no native camera binding')
    return value


def capture_config(service, *args, **kwargs):
    require(service in HTTP_SENSORS, 'camera-b uses only the native World frame path')
    return _base.capture_config(service, *args, **kwargs)


def native_bridge_config(run_id, scene_identity, *, source_id, metadata_url):
    scene = validate_scene_identity(run_id, scene_identity)
    require(isinstance(source_id, str) and source_id.startswith('sensorlink:spiffe://wendy.sh/tenant/')
            and source_id.endswith(':' + str(CHANNEL)), 'native source binding required')
    return {'commission': {'run_id': run_id, 'boot_id': scene['boot_id'],
                'package_sha256': scene['package_sha256'], 'site': scene['site'],
                'frame': scene['frame'], 'sensor_id': NATIVE_CAMERA,
                'simulation_anchor_unix_ns': scene['anchor_unix_ns']},
            'bounds': {'duration_ms': 60000, 'max_frames': 2, 'request_timeout_ms': 3000, 'ttl_ms': 3000},
            'isolated_simulation_network': True, 'metadata_url': _base._source_url(metadata_url),
            'metadata_read_token_file': '/config/metadata-read-token',
            'channel_id': CHANNEL, 'source_id': source_id, 'poll_interval_ms': 125}


def native_source_config(run_id, scene_identity, *, scene_url, scene_token_file, metadata_token_file,
                         output_directory):
    scene = validate_scene_identity(run_id, scene_identity)
    commission = {'run_id': run_id, 'boot_id': scene['boot_id'], 'package_sha256': scene['package_sha256'],
                  'site': scene['site'], 'frame': scene['frame'], 'sensor_id': NATIVE_CAMERA,
                  'simulation_anchor_unix_ns': scene['anchor_unix_ns']}
    return {'config_directory': config_path('a'), 'output_directory': _base._path(output_directory),
            'source': {'commission': commission,
                'bounds': {'duration_ms': 180000, 'max_frames': 2, 'request_timeout_ms': 3000, 'ttl_ms': 3000},
                'isolated_simulation_network': True, 'scene_url': _base._source_url(scene_url),
                'scene_read_token_file': _base._path(scene_token_file),
                'metadata_read_token_file': _base._path(metadata_token_file),
                'tcp_listen': '127.0.0.1:50060', 'metadata_listen': '0.0.0.0:50061',
                'channel_id': CHANNEL, 'chunk_bytes': 16384, 'jpeg_quality': 80,
                'poll_interval_ms': 125, 'width': 320, 'height': 240}}


def volume_names(run_id, node, service):
    sample_domain(run_id)
    require(node in SERVICES and service in SERVICES[node], 'service belongs to another node')
    return {kind: 'world-nativesensorfusion-' + run_id + '-' + node + '-' + service + '-' + kind
            for kind in ('config', 'proofs')}


_base.volume_names = volume_names


def app_manifest(pair, run_id, node):
    value = _base.app_manifest(pair, run_id, node)
    if node == 'a':
        # The source metadata endpoint is in A's host namespace. Camera bytes
        # still arrive only through its scoped World socket, never this URL.
        value['services'][NATIVE_CAMERA]['entitlements'].append({'type': 'network', 'mode': 'bridge'})
    return value
