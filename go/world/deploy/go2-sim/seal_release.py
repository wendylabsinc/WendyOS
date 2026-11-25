"""Build-time inventory for the compiled ROS helper, never run configuration."""
import hashlib
import json
from pathlib import Path
from boot import create_durable, read_regular

def seal(app=Path('/opt/world')):
    release = read_regular(app / 'release.json')
    helper = read_regular(app / 'world-go2-sensors', 16 << 20)
    cloud_helper = read_regular(app / 'world-go2-cloud-live', 16 << 20)
    output = {'schema': 'wendy.world.go2-native-build.v2',
              'release_id': hashlib.sha256(release).hexdigest(),
              'helper': {'sha256': hashlib.sha256(helper).hexdigest(), 'bytes': len(helper)},
              'cloud_helper': {'sha256': hashlib.sha256(cloud_helper).hexdigest(), 'bytes': len(cloud_helper)}}
    create_durable(app / 'native-digest.json', (json.dumps(output, separators=(',', ':')) + '\n').encode())

if __name__ == '__main__':
    seal()
