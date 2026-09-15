import json
import pytest
from reference_bank import load_training_sources,sha


@pytest.fixture
def bank(tmp_path):
    data=tmp_path/'data';cache=tmp_path/'cache';cache.mkdir()
    entries=[];order=[]
    for i in range(768):
        name=f'episode-{i:04d}';path=data/'dataset'/name/'episode.json'
        path.parent.mkdir(parents=True)
        source=str(tmp_path/'sources'/name);order.append(source)
        path.write_text(json.dumps(dict(split='train',trajectory=source)))
        entries.append(dict(name=name,split='train',episode_sha256=sha(path)))
    # The loader must not open the absent held-out episode files.
    entries.extend([dict(name='validation-hidden',split='validation'),dict(name='test-hidden',split='test')])
    manifest=cache/'manifest.json';manifest.write_text(json.dumps(dict(train=768,validation=106,test_used=0,episodes=entries)))
    return data,cache,dict(cache_manifest_sha256=sha(manifest),reference_order=order)


def test_only_training_references_are_resolved(bank):
    data,cache,contract=bank
    sources=load_training_sources(data,cache,contract)
    assert len(sources)==768
    assert [str(p) for p in sources]==contract['reference_order']


def test_tampered_episode_cannot_enter_training(bank):
    data,cache,contract=bank
    p=data/'dataset/episode-0000/episode.json'
    p.write_text(p.read_text().replace('train','test'))
    with pytest.raises(ValueError,match='hash mismatch'):load_training_sources(data,cache,contract)


def test_reordered_parent_bank_is_rejected(bank):
    data,cache,contract=bank
    contract['reference_order'].reverse()
    with pytest.raises(ValueError,match='Reference order'):load_training_sources(data,cache,contract)
