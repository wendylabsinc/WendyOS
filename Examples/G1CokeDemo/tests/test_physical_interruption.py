"""Actual process/flock/thread cleanup, with a private socket backend, never DDS."""
import socket
import signal
import subprocess
import sys
from types import SimpleNamespace

import pytest

from physical_io.unitree_io import CommandProfile, ExactPolicyUnitreeIO, HAND_MAPPING_ID, IoConfig


class PrivateSocketBackend:
    def __init__(self, interruption=None):
        self.sender, self.receiver = socket.socketpair()
        self.sender.settimeout(2)
        self.receiver.settimeout(2)
        self.interruption = interruption
        self.raised = None
        self.closed = False

    def subscribe(self, body, left, right):
        def motors(count):
            return [SimpleNamespace(q=0, dq=0, motorstate=0) for _ in range(count)]
        body(SimpleNamespace(motor_state=motors(29), mode_machine=1, wireless_remote=bytes(24)))
        left(SimpleNamespace(motor_state=motors(7), error=[0]))
        right(SimpleNamespace(motor_state=motors(7), error=[0]))

    def enable_publishers(self):
        # Only this explicitly supplied socket backend can be armed. No SDK,
        # network interface, radio, command publisher or robot is initialized.
        assert self.sender.fileno() >= 0

    def release(self):
        self.sender.sendall(b'owned-release')
        try:
            if self.interruption == 'keyboard':
                signal.raise_signal(signal.SIGINT)
            elif self.interruption == 'system-exit':
                sys.exit(19)
        except (KeyboardInterrupt, SystemExit) as error:
            self.raised = error
            raise

    def close(self):
        self.sender.close()
        self.receiver.close()
        self.closed = True


def probe_lock(path, expected):
    child = '''import fcntl,sys
with open(sys.argv[1], 'rb') as stream:
 try: fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(73)
'''
    result = subprocess.run([sys.executable, '-c', child, str(path)], timeout=3, capture_output=True)
    assert result.returncode == expected, f'actual competing-process lock result {result.returncode}: {result.stderr!r}'


@pytest.mark.parametrize('interruption', ['keyboard', 'system-exit'])
def test_interruption_propagates_after_owned_process_lock_and_watchdog_drain(tmp_path, interruption):
    path = tmp_path / 'synthetic-owner.lock'
    backend = PrivateSocketBackend(interruption)
    config = IoConfig(interface='owned-socket-only', motion_enabled=True,
                      verified_hand_mapping_id=HAND_MAPPING_ID, allowed_mode_machine=(1,),
                      ownership_path=path)
    # Frozen input freshness avoids making OS scheduling of the competing
    # process part of this exception-cleanup test. The watchdog is a real thread.
    owned = ExactPolicyUnitreeIO(config, backend=backend, monotonic=lambda: 100.0)
    profile = CommandProfile('synthetic-socket-test', 'a' * 64, True, (0,) * 17, (0,) * 17, (0,) * 7, (0,) * 7, .1)
    try:
        owned.arm('first-fixture-owner', profile)
        worker = owned._watchdog_thread
        assert worker is not None and worker.is_alive()
        probe_lock(path, 73)
        caught = None
        try:
            owned.disarm()
        except BaseException as error:
            caught = error
        assert backend.raised is not None and caught is backend.raised, f'PHYSICAL_INTERRUPTION_SWALLOWED expected original {interruption}, got {caught!r}'
        if interruption == 'keyboard':
            assert isinstance(caught, KeyboardInterrupt)
        else:
            assert isinstance(caught, SystemExit) and caught.code == 19
        assert backend.receiver.recv(64) == b'owned-release'
        assert not worker.is_alive(), 'actual watchdog was not joined before interruption propagation'
        assert not owned.status()['publishers_armed'] and owned.status()['owner'] is None
        probe_lock(path, 0)
        owned.close()
        assert backend.closed
        # The released process-local registry must also admit a fresh lifecycle.
        recovered_backend = PrivateSocketBackend()
        recovered = ExactPolicyUnitreeIO(config, backend=recovered_backend, monotonic=lambda: 100.0)
        try:
            recovered.arm('second-fixture-owner', profile)
            recovered_worker = recovered._watchdog_thread
            recovered.disarm()
            assert recovered_backend.receiver.recv(64) == b'owned-release'
            assert not recovered_worker.is_alive()
            probe_lock(path, 0)
        finally:
            recovered.close()
        print(f'PHYSICAL_INTERRUPTION_CLEANUP_VERIFIED {interruption} actual flock, socket and watchdog; no DDS')
    finally:
        owned.close()
