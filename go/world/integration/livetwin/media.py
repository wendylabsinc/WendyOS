"""Fetch optional original evidence without delaying current replica display."""
from concurrent.futures import ThreadPoolExecutor
import subprocess

from integration.remotemediafusion import control, model


class EvidenceCache:
    def __init__(self, container, path, save):
        self.container, self.path, self.save = container, path, save
        self.worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='native-twin-evidence')
        self.pending = None
        self.cache, self.attempted = {}, set()

    def _fetch(self, digest):
        result = subprocess.run(['docker', 'exec', self.container, 'python3', '-c',
            control.MATCH_PUBLICATIONS, self.path, model.json_bytes([digest]).decode()],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=3, check=False)
        if result.returncode != 0:
            return None
        return model.strict_json(result.stdout, 4 << 20)

    def available(self, digests):
        """Return only completed exact matches; never wait for a fetch."""
        if self.pending is not None and self.pending[1].done():
            digest, future = self.pending
            self.pending = None
            try:
                origin = future.result()
            except Exception:
                origin = None
            if origin is not None:
                self.save('live/origin-' + digest + '.json', origin)
                self.cache[digest] = origin[digest]['publication']['media']
        if self.pending is None:
            missing = [digest for digest in digests if digest not in self.attempted]
            if missing and len(self.attempted) < 64:
                digest = missing[0]
                self.attempted.add(digest)
                self.pending = digest, self.worker.submit(self._fetch, digest)
        return [item for digest in digests for item in self.cache.get(digest, [])]

    def close(self):
        self.worker.shutdown(wait=True, cancel_futures=True)
