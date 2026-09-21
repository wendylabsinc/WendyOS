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
