"""Resolve only hash-bound training references; no validation/test rollouts."""
import json,hashlib
from pathlib import Path


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def load_training_sources(data, cache, contract):
    manifest_path=Path(cache)/'manifest.json'
    if sha(manifest_path)!=contract['cache_manifest_sha256']:
        raise ValueError('Training manifest differs from parent checkpoint')
    manifest=json.loads(manifest_path.read_text())
    if (manifest['train'],manifest['validation'],manifest['test_used'])!=(768,106,0):
        raise ValueError('Unexpected corpus split')
    entries=sorted((e for e in manifest['episodes'] if e['split']=='train'),key=lambda e:e['name'])
    sources=[]
    for entry in entries:
        path=Path(data)/'dataset'/entry['name']/'episode.json'
        if sha(path)!=entry['episode_sha256']:
            raise ValueError(f'Episode metadata hash mismatch: {path}')
        episode=json.loads(path.read_text())
        if episode['split']!='train':raise ValueError('Held-out episode in training bank')
        sources.append(Path(episode['trajectory']))
    if len(sources)!=768 or len(set(sources))!=768:
        raise ValueError('Missing or duplicate training reference')
    if [str(p) for p in sources]!=contract['reference_order']:
        raise ValueError('Reference order differs from parent checkpoint')
    return sources
