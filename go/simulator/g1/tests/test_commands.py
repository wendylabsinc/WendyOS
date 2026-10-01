"""DDS queue age and publisher ownership across resets, without a ROS daemon."""

from types import SimpleNamespace

import pytest

from g1_sim.commands import ROSCommands
from g1_sim.simulation import Simulation


class Clock:
    now = 1_000_000_000_000

    def ns(self):
        return self.now

    def seconds(self):
        return self.now / 1e9

    def tick(self):
        self.now += 50_000_000


@pytest.fixture
def bus():
    clock = Clock()
    sim = Simulation(monotonic=clock.seconds)
    runtime = SimpleNamespace(sim=sim, ensure_running=lambda: None,
                              command=lambda values, token: sim.command_velocity(*values, token))
    return ROSCommands(runtime, "/unused", monotonic_ns=clock.ns, wall_ns=clock.ns), clock


def envelope(clock, gid="a" * 48):
    return {"kind": "twist", "publisher_gid": gid, "received_ns": clock.ns(),
            "source_timestamp_ns": clock.ns(), "velocity": [0.3, 0.0, 0.0]}


def test_reset_blocks_still_running_writer_even_after_explicit_regrant(bus):
    commands, clock = bus
    assert not commands.admit(envelope(clock))
    commands.grant("a" * 48)
    clock.tick()
    assert commands.admit(envelope(clock))
    commands.revoke()
    commands.runtime.sim.reset()
    clock.tick()
    assert not commands.admit(envelope(clock))
    with pytest.raises(PermissionError, match="restart"):
        commands.grant("a" * 48)
    assert not commands.admit(envelope(clock, "b" * 48))
    commands.grant("b" * 48)
    clock.tick()
    assert commands.admit(envelope(clock, "b" * 48))
    assert not commands.admit(envelope(clock))


def test_queueing_does_not_renew_command_lifetime(bus):
    commands, clock = bus
    commands.admit(envelope(clock))
    commands.grant("a" * 48)
    clock.tick()
    queued = envelope(clock)
    clock.tick()
    assert commands.admit(queued)
    assert commands.runtime.sim._last_received == queued["received_ns"] / 1e9
    for _ in range(4):
        clock.tick()
    with pytest.raises(ValueError, match="expired ingress"):
        commands.admit(queued)


def test_old_dds_sample_cannot_cross_new_grant(bus):
    commands, clock = bus
    queued = envelope(clock)
    commands.admit(queued)
    clock.tick()
    commands.grant("a" * 48)
    clock.tick()
    queued["received_ns"] = clock.ns()
    assert not commands.admit(queued)
    assert commands.runtime.sim.command.tolist() == [0.0, 0.0, 0.0]


def test_browser_and_ros_cannot_own_controller_together(bus):
    commands, clock = bus
    commands.admit(envelope(clock))
    commands.runtime.sim.arm()
    with pytest.raises(PermissionError, match="another"):
        commands.grant("a" * 48)


@pytest.fixture
def automatic_bus(bus):
    commands, clock = bus
    commands.auto_control = True
    return commands, clock


def test_new_driving_publisher_takes_control_once(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    browser = sim.arm()
    assert not commands.admit(envelope(clock))
    assert commands.owner == 'a' * 48
    assert sim.owner != browser
    assert sim.command.tolist() == [0, 0, 0]
    clock.tick()
    assert commands.admit(envelope(clock))
    clock.tick()
    assert not commands.admit(envelope(clock, 'b' * 48))
    replacement = sim.owner
    assert commands.owner == 'b' * 48
    assert sim.command.tolist() == [0, 0, 0]
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert commands.admit(envelope(clock, 'b' * 48))
    assert sim.owner == replacement


def test_sdk_query_before_velocity_does_not_consume_handoff(automatic_bus):
    commands, clock = automatic_bus
    received = []
    commands.native_handler = lambda packet, owned: received.append(owned) or owned
    commands.native_auto_grant = lambda packet: packet.get('valid_velocity', False)
    query = envelope(clock) | {'kind': 'sport'}
    assert not commands.admit(query)
    assert commands.owner is None
    clock.tick()
    velocity = envelope(clock) | {'kind': 'sport', 'valid_velocity': True}
    # Even a slightly future-dated sample cannot drive on the handoff request.
    velocity['source_timestamp_ns'] += 40_000_000
    assert not commands.admit(velocity)
    assert commands.owner == 'a' * 48
    assert received == [False, False], 'the SDK must receive a reply to its first velocity request'
    clock.tick()
    assert commands.admit(envelope(clock) | {'kind': 'sport', 'valid_velocity': True})
    assert received[-1] is True


@pytest.mark.parametrize('action', ['reset', 'pause', 'release'])
def test_old_publishers_stay_blocked_new_run_gets_control(automatic_bus, action):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    commands.admit(envelope(clock))
    token = sim.owner
    commands.revoke()
    if action == 'reset':
        sim.reset()
    elif action == 'pause':
        sim.pause()
        sim.resume()
    else:
        sim.release(token)
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    assert commands.status()['sources'][0]['requires_restart']
    assert not commands.admit(envelope(clock, 'b' * 48))
    assert commands.owner == 'b' * 48


@pytest.mark.parametrize('mode', ['paused', 'fallen', 'damping', 'fault', 'zero_torque', 'lowlevel'])
def test_no_delayed_handoff_after_recovery(automatic_bus, mode):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    sim.mode = mode
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    sim.mode = 'standing'
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    assert not commands.admit(envelope(clock, 'b' * 48))
    assert commands.owner == 'b' * 48


@pytest.mark.parametrize('velocity', [[True, 0, 0], [1.1, 0, 0], [0, .31, 0], [0, 0, .21],
                                    [float('nan'), 0, 0], [10**400, 0, 0], [0, 0]])
def test_invalid_new_source_cannot_displace_owner(automatic_bus, velocity):
    commands, clock = automatic_bus
    token = commands.runtime.sim.arm()
    with pytest.raises(ValueError):
        commands.admit(envelope(clock) | {'velocity': velocity})
    assert commands.runtime.sim.owner == token
    assert not commands.sources


def test_lowlevel_and_faulted_runtime_keep_their_owner(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    token = sim.arm('lowlevel')
    assert not commands.admit(envelope(clock))
    assert sim.owner == token
    sim.reset()
    commands.runtime.error = 'fault'
    token = sim.arm()
    assert not commands.admit(envelope(clock, 'b' * 48))
    assert sim.owner == token
    commands.runtime.error = None
    clock.tick()
    assert not commands.admit(envelope(clock, 'b' * 48))
    assert sim.owner == token


def test_publisher_cannot_change_command_kind(automatic_bus):
    commands, clock = automatic_bus
    commands.admit(envelope(clock))
    clock.tick()
    with pytest.raises(ValueError, match='kind changed'):
        commands.admit(envelope(clock) | {'kind': 'sport'})
