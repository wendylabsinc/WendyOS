"""Exercise an explicitly selected virtual arm endpoint, never device hardware."""
import argparse
import json
import time
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


def smoke(url, vm_name):
    parsed = urlsplit(url)
    if parsed.scheme != 'http' or parsed.hostname != '127.0.0.1' or not parsed.port:
        raise ValueError('use the verified loopback sandbox URL')

    def request(path, body=None):
        req = Request(url.rstrip('/') + path, data=None if body is None else json.dumps(body).encode(),
                      headers={'Content-Type': 'application/json'})
        with urlopen(req, timeout=10) as response:
            data = response.read()
            return json.loads(data) if response.headers['Content-Type'].startswith('application/json') else data

    before = request('/api/status')
    assert before['robot_kind'] == 'so101' and before['virtual_only'] and before['simulation']
    assert before['healthy'] and before['ready'] and before['vm_name'] == vm_name
    assert len(before['joints']) == 6
    scene = request('/api/scene')
    pose = request('/api/scene/state')
    assert scene['id'] == pose['scene_id'] and pose['valid']
    assert scene['robot_body'] >= 0 and len(scene['meshes']) >= 10
    reset = request('/api/reset', {})
    assert reset['epoch'] > before['epoch']
    target = request('/api/pose', {'joints_deg': {'shoulder_pan': 20, 'gripper': 30}})
    assert abs(target['joints'][0]['target_deg'] - 20) < 1e-6
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        time.sleep(.2)
        moved = request('/api/status')
        if moved['joints'][0]['position_deg'] > 5:
            break
    assert moved['joints'][0]['position_deg'] > 5
    try:
        request('/api/pose', {'joints_deg': {'shoulder_pan': 10000}})
    except HTTPError as error:
        assert error.code == 422
    else:
        raise AssertionError('unbounded target accepted')
    paused = request('/api/pause', {'paused': True})
    time.sleep(.3)
    assert request('/api/status')['simulation_seconds'] == paused['simulation_seconds']
    jpeg = request('/camera.jpg')
    assert jpeg.startswith(b'\xff\xd8') and len(jpeg) > 1000
    final = request('/api/reset', {})
    assert final['epoch'] > paused['epoch'] and not final['paused'] and not final['demo']
    print(json.dumps({'vm_name': vm_name, 'source_digest': final['source_digest'],
                      'healthy': final['healthy'], 'ready': final['ready'],
                      'epoch_before': before['epoch'], 'epoch_after': final['epoch'],
                      'joint_count': len(final['joints']), 'scene_mesh_count': len(scene['meshes']),
                      'moved_shoulder_deg': moved['joints'][0]['position_deg'],
                      'rendered_jpeg_bytes': len(jpeg)}, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('url')
    parser.add_argument('--vm-name', required=True)
    args = parser.parse_args()
    smoke(args.url, args.vm_name)
