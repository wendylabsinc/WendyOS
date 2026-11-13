"""Run after the normal shipped-image pytest suite. Restore original 2245 bounds."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from so101_sim import server


def main():
    if os.getenv('SO101_RENDER_TEST') != '1':
        raise RuntimeError('real shipped renderer must be required')
    root = Path(server.__file__).resolve().parents[1]
    source = (root / 'so101_sim/server.py').read_text()
    before = """        low, high = np.rad2deg(sim.limits[i])
        if not low <= degrees <= high:
            raise HTTPException(422, f'Out of range: {name}')
        commands.append((i, float(np.clip(math.radians(degrees), *sim.limits[i]))))"""
    after = """        radians = math.radians(degrees)
        if not sim.limits[i, 0] <= radians <= sim.limits[i, 1]:
            raise HTTPException(422, f'Out of range: {name}')
        commands.append((i, radians))"""
    if source.count(before) != 1:
        raise RuntimeError('original joint-bound boundary changed')
    with tempfile.TemporaryDirectory(prefix='so101-http-control-') as temporary:
        work = Path(temporary)
        package = work / 'so101_sim'
        package.mkdir()
        for name in ('__init__.py', 'scene.py'):
            shutil.copyfile(root / 'so101_sim' / name, package / name)
        (package / 'server.py').write_text(source.replace(before, after))
        (work / 'model').symlink_to(root / 'model', target_is_directory=True)
        shutil.copyfile(root / 'assets.lock.json', work / 'assets.lock.json')
        env = dict(os.environ, PYTHONPATH=str(work), PYTEST_DISABLE_PLUGIN_AUTOLOAD='1')
        command = [sys.executable, str(Path(__file__).with_name('test_http_lifecycle.py'))]
        # The child runs only Python/ASGI/MuJoCo threads; no subprocess is started
        # by this test. subprocess.run kills and reaps that exact child on timeout.
        result = subprocess.run(command, cwd=work, env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, timeout=180)
        print(result.stdout)
        final_line = result.stdout.strip().splitlines()[-1] if result.stdout.strip() else ''
        if (result.returncode != 1
                or not final_line.startswith('AssertionError: SO101_REPORTED_BOUND_REJECTED ')
                or 'SO101_HTTP_LIFECYCLE_VERIFIED' in result.stdout):
            raise RuntimeError('original joint-bound control did not fail at the reported endpoint')
        print('SO101_HTTP_BOUND_CONTROL_VERIFIED source_sha256=' + hashlib.sha256(source.encode()).hexdigest())
    return 0

if __name__ == '__main__':
    sys.exit(main())
