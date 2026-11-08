#!/usr/bin/env python3
"""Build deterministic Linux service layers for installation by Apple clients."""
import fcntl
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile

os.environ['PATH'] = '/opt/homebrew/bin:/usr/local/bin:' + os.environ.get('PATH', '')

root = Path(__file__).resolve().parents[3]
cache = root / 'go/macos/Build/DigitalTwin'
cache.mkdir(parents=True, exist_ok=True)
destination = Path(os.environ['TARGET_BUILD_DIR']) / os.environ['UNLOCALIZED_RESOURCES_FOLDER_PATH'] / 'DigitalTwin'
destination.mkdir(parents=True, exist_ok=True)
with (cache / '.lock').open('w') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    for arch in ('arm64', 'amd64'):
        binary = cache / ('wendy-twin-' + arch)
        env = dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=arch)
        subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w -buildid=',
                        '-o', str(binary), './go/cmd/wendy-twin'], cwd=root, env=env, check=True)
        # Ship the native worker and its exact Linux libraries with the service.
        # No solver implementation or OpenCV dependency lives in a web client.
        runtime = cache / ('vision-' + arch)
        if runtime.exists():
            shutil.rmtree(runtime)
        subprocess.run(['docker', 'buildx', 'build', '--platform', 'linux/' + arch,
                        '--output', 'type=local,dest=' + str(runtime),
                        str(root / 'go/internal/twinvision/native')], check=True)
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name, mode in [('usr', 0o755), ('usr/local', 0o755), ('usr/local/bin', 0o755), ('data', 0o755), ('tmp', 0o1777)]:
                entry = tarfile.TarInfo(name); entry.type = tarfile.DIRTYPE; entry.mode = mode
                archive.addfile(entry)
            for path in sorted(runtime.rglob('*')):
                if not path.is_file():
                    continue
                entry = tarfile.TarInfo(str(path.relative_to(runtime)))
                entry.mode = path.stat().st_mode & 0o777
                contents = path.read_bytes(); entry.size = len(contents)
                archive.addfile(entry, io.BytesIO(contents))
            data = binary.read_bytes()
            entry = tarfile.TarInfo('usr/local/bin/wendy-twin'); entry.mode = 0o755; entry.size = len(data)
            archive.addfile(entry, io.BytesIO(data))
        raw = buffer.getvalue()
        layer = gzip.compress(raw, mtime=0)
        digest = 'sha256:' + hashlib.sha256(layer).hexdigest()
        diff_id = 'sha256:' + hashlib.sha256(raw).hexdigest()
        config = json.loads((root / 'go/apps/digital-twin/wendy.json').read_text())
        config['version'] = '0.1.0-' + hashlib.sha256(raw + json.dumps(config, sort_keys=True).encode()).hexdigest()[:16]
        image = {'architecture': arch, 'os': 'linux',
                 'config': {'Entrypoint': ['/usr/local/bin/wendy-twin'], 'Env': ['WENDY_TWIN_DATA=/data'], 'WorkingDir': '/'},
                 'rootfs': {'type': 'layers', 'diff_ids': [diff_id]}}
        metadata = {'digest': digest, 'diffID': diff_id, 'size': len(layer), 'imageConfig': image, 'appConfig': config}
        (cache / (arch + '.tar.gz')).write_bytes(layer)
        (cache / (arch + '.json')).write_text(json.dumps(metadata, separators=(',', ':'), sort_keys=True))
        for suffix in ('.tar.gz', '.json'):
            shutil.copyfile(cache / (arch + suffix), destination / (arch + suffix))
        print('Bundled Linux twin', arch, config['version'], len(layer), 'bytes')
