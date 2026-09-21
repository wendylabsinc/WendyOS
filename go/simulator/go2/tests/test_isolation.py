"""Bootstrap must not evade an ownership or partial-install failure."""
import json
import subprocess
from types import SimpleNamespace

import pytest
from go2_sim import isolation, isolation_legacy


def test_unsupported_nft_kernel_uses_verified_legacy_result(monkeypatch):
    def unavailable(*args, **kwargs):
        raise subprocess.CalledProcessError(1, args[0], stderr='')
    expected = {'dds_isolation': 'udp-rtps-loopback', 'backend': 'iptables-legacy'}
    monkeypatch.setattr(isolation.subprocess, 'run', unavailable)
    monkeypatch.setattr(isolation_legacy, 'install', lambda: expected)
    assert isolation.install() is expected


def test_foreign_nft_table_is_never_bypassed_with_legacy(monkeypatch):
    replies = iter([SimpleNamespace(stdout=json.dumps({'nftables': [
        {'table': {'family': 'inet', 'name': 'wendy_go2'}}]})),
        SimpleNamespace(stdout='table inet wendy_go2 {\n comment "somebody else"\n}')])
    monkeypatch.setattr(isolation.subprocess, 'run', lambda *a, **kw: next(replies))
    monkeypatch.setattr(isolation_legacy, 'install', lambda: pytest.fail('ownership was bypassed'))
    with pytest.raises(RuntimeError, match='unknown owner'):
        isolation.install()


def test_nft_transaction_failure_does_not_switch_backends(monkeypatch):
    calls = []
    def run(command, **kwargs):
        calls.append(command)
        if len(calls) == 1:
            return SimpleNamespace(stdout='{"nftables": []}')
        raise subprocess.CalledProcessError(1, command, stderr='transaction refused')
    monkeypatch.setattr(isolation.subprocess, 'run', run)
    monkeypatch.setattr(isolation_legacy, 'install', lambda: pytest.fail('transaction failure was bypassed'))
    with pytest.raises(subprocess.CalledProcessError, match='non-zero exit'):
        isolation.install()
    assert calls[-1] == ['nft', '--file', '-']


def test_failed_legacy_bootstrap_propagates(monkeypatch):
    def unavailable(*args, **kwargs):
        raise subprocess.CalledProcessError(1, args[0], stderr='')
    def legacy_failure():
        raise RuntimeError('IPv6 verification failed')
    monkeypatch.setattr(isolation.subprocess, 'run', unavailable)
    monkeypatch.setattr(isolation_legacy, 'install', legacy_failure)
    with pytest.raises(RuntimeError, match='IPv6 verification failed'):
        isolation.install()
