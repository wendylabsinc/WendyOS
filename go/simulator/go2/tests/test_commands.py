"""DDS queue age and publisher ownership across resets, without a ROS daemon."""

from types import SimpleNamespace

import pytest

from go2_sim.commands import ROSCommands
from go2_sim.simulation import Simulation, VELOCITY_LIMITS


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


def test_node_labels_are_display_metadata_and_do_not_grant_control(bus):
    commands, clock = bus
    packet = envelope(clock) | {"node_name": "wendy_go2_patrol", "node_namespace": "/apps/robot"}
    assert not commands.admit(packet)
    source = commands.status()["sources"][0]
    assert source["node_name"] == "wendy_go2_patrol"
    assert source["node_namespace"] == "/apps/robot"
    assert commands.owner is None
    with pytest.raises(ValueError, match="discovered"):
        commands.grant("wendy_go2_patrol")
    commands.grant(packet["publisher_gid"])
    clock.tick()
    assert commands.admit(envelope(clock) | {"node_name": "another_name", "node_namespace": "/"})
    assert commands.owner == packet["publisher_gid"]


@pytest.mark.parametrize("metadata", [
    {"node_name": "not/a/node", "node_namespace": "/"},
    {"node_name": "<script>", "node_namespace": "/"},
    {"node_name": "bad\nname", "node_namespace": "/"},
    {"node_name": "a" * 129, "node_namespace": "/"},
    {"node_name": "app", "node_namespace": "/" + "a" * 128},
    {"node_name": "app", "node_namespace": "relative"},
    {"node_name": "app", "node_namespace": "/trailing/"},
    {"node_name": "app", "node_namespace": "/double//slash"},
    {"node_name": "app", "node_namespace": "/9invalid"},
    {"node_name": "app"}, {"node_namespace": "/"},
    {"node_name": 7, "node_namespace": "/"},
])
def test_invalid_optional_labels_never_change_command_admission(bus, metadata):
    commands, clock = bus
    commands.admit(envelope(clock))
    commands.grant("a" * 48)
    clock.tick()
    assert commands.admit(envelope(clock) | metadata)
    source = commands.status()["sources"][0]
    assert "node_name" not in source
    assert "node_namespace" not in source


def test_same_named_nodes_keep_distinct_publisher_grants_and_reset_fences(bus):
    commands, clock = bus
    metadata = {"node_name": "same_node", "node_namespace": "/"}
    commands.admit(envelope(clock, "a" * 48) | metadata)
    commands.admit(envelope(clock, "b" * 48) | metadata)
    assert len(commands.status()["sources"]) == 2
    commands.grant("a" * 48)
    clock.tick()
    assert not commands.admit(envelope(clock, "b" * 48) | metadata)
    assert commands.admit(envelope(clock, "a" * 48) | metadata)
    commands.revoke()
    commands.runtime.sim.reset()
    with pytest.raises(PermissionError, match="restart"):
        commands.grant("a" * 48)


def test_reordered_packets_cannot_replace_a_publisher_label(bus):
    commands, clock = bus
    packet = envelope(clock) | {"node_name": "new_name", "node_namespace": "/"}
    commands.admit(packet)
    assert not commands.admit(packet | {"node_name": "old_name"})
    assert commands.status()["sources"][0]["node_name"] == "new_name"


def test_optional_metadata_omission_preserves_only_the_same_endpoint_label(bus):
    commands, clock = bus
    commands.admit(envelope(clock) | {"node_name": "app", "node_namespace": "/examples"})
    clock.tick()
    commands.admit(envelope(clock))
    assert commands.status()["sources"][0]["node_name"] == "app"
    assert commands.status()["sources"][0]["node_namespace"] == "/examples"
    commands.admit(envelope(clock, "b" * 48))
    assert "node_name" not in commands.status()["sources"][1]
    clock.tick()
    commands.admit(envelope(clock) | {"node_name": "invalid/name", "node_namespace": "/"})
    assert "node_name" not in commands.status()["sources"][0]


@pytest.fixture
def automatic_bus(bus):
    commands, clock = bus
    commands.auto_control = True
    return commands, clock


def test_automatic_control_is_opt_in(bus):
    commands, clock = bus
    assert commands.status()["auto_control"] is False
    assert not commands.admit(envelope(clock))
    assert commands.runtime.sim.owner is None


def test_new_app_takes_control_once_and_fences_its_discovery_packet(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    assert commands.status()["auto_control"] is True
    assert not commands.admit(envelope(clock))
    first_token = sim.owner
    assert commands.owner == "a" * 48
    assert first_token is not None
    assert sim.command.tolist() == [0, 0, 0]
    clock.tick()
    assert commands.admit(envelope(clock))
    assert sim.command.tolist() == [0.3, 0, 0]

    clock.tick()
    assert not commands.admit(envelope(clock, "b" * 48))
    assert commands.owner == "b" * 48
    assert sim.owner != first_token
    assert sim.command.tolist() == [0, 0, 0]
    with pytest.raises(PermissionError, match="expired"):
        sim.command_velocity(0.3, 0, 0, first_token)
    for _ in range(3):
        clock.tick()
        assert not commands.admit(envelope(clock))
        assert commands.admit(envelope(clock, "b" * 48))
        assert commands.owner == "b" * 48


def test_discovery_with_tolerated_future_dds_timestamp_stays_fenced(automatic_bus):
    commands, clock = automatic_bus
    packet = envelope(clock) | {"source_timestamp_ns": clock.ns() + 40_000_000}
    assert not commands.admit(packet)
    assert commands.owner == packet["publisher_gid"]
    assert commands.runtime.sim.command.tolist() == [0, 0, 0]
    clock.tick()
    assert commands.admit(envelope(clock))


def test_new_app_takes_browser_control_and_invalidates_browser_token(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    browser_token = sim.arm()
    sim.command_velocity(0.2, 0, 0, browser_token)
    sim.mode = "moving"
    assert not commands.admit(envelope(clock))
    assert commands.owner == "a" * 48
    assert sim.owner != browser_token
    assert sim.command.tolist() == [0, 0, 0]
    with pytest.raises(PermissionError, match="expired"):
        sim.command_velocity(0.2, 0, 0, browser_token)
    clock.tick()
    assert commands.admit(envelope(clock))


@pytest.mark.parametrize("change", [
    {"velocity": [0.81, 0, 0]}, {"velocity": [0, -0.51, 0]},
    {"velocity": [0, 0, 1.01]}, {"velocity": [True, 0, 0]},
    {"velocity": [float("nan"), 0, 0]}, {"velocity": [float("inf"), 0, 0]},
    {"velocity": [10 ** 400, 0, 0]}, {"velocity": [0.2, 0]},
    {"velocity": "0.2,0,0"}, {"publisher_gid": "invalid"},
    {"received_ns": 0}, {"source_timestamp_ns": 0},
    {"received_ns": True}, {"source_timestamp_ns": True},
])
def test_invalid_new_app_never_displaces_current_owner(automatic_bus, change):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    browser_token = sim.arm()
    sim.command_velocity(0.2, 0, 0, browser_token)
    with pytest.raises(ValueError):
        commands.admit(envelope(clock) | change)
    assert sim.owner == browser_token
    assert sim.command.tolist() == [0.2, 0, 0]
    assert commands.sources == {}
    # A valid first sample may still request control after malformed traffic.
    assert not commands.admit(envelope(clock))
    assert commands.owner == "a" * 48


def test_velocity_limit_values_can_request_automatic_control(automatic_bus):
    commands, clock = automatic_bus
    values = VELOCITY_LIMITS.tolist()
    assert not commands.admit(envelope(clock) | {"velocity": values})
    clock.tick()
    assert commands.admit(envelope(clock) | {"velocity": values})
    assert commands.runtime.sim.command.tolist() == values


@pytest.mark.parametrize("mode", [
    "paused", "fallen", "damping", "fault", "standing_up", "standing_down", "lying", "lowlevel",
])
def test_unsafe_mode_consumes_new_app_attempt_without_granting_after_recovery(automatic_bus, mode):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    sim.mode = mode
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    sim.mode = "standing"
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    assert not commands.admit(envelope(clock, "b" * 48))
    assert commands.owner == "b" * 48


def test_automatic_control_does_not_displace_low_level_owner(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    token = sim.arm("lowlevel")
    assert not commands.admit(envelope(clock))
    assert sim.owner == token
    assert commands.owner is None


def test_automatic_control_rejects_inconsistent_low_level_mode(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    sim.control_mode = "lowlevel"
    assert not commands.admit(envelope(clock))
    assert sim.owner is None


@pytest.mark.parametrize("failed_ensure", [False, True])
def test_runtime_failure_preserves_owner_and_consumes_new_app_attempt(automatic_bus, failed_ensure):
    commands, clock = automatic_bus
    runtime = commands.runtime
    token = runtime.sim.arm()
    if failed_ensure:
        def stopped():
            raise RuntimeError("physics runtime is not running")
        runtime.ensure_running = stopped
        with pytest.raises(RuntimeError, match="not running"):
            commands.admit(envelope(clock))
        runtime.ensure_running = lambda: None
    else:
        runtime.error = "physics failed"
        assert not commands.admit(envelope(clock))
        runtime.error = None
    assert runtime.sim.owner == token
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert runtime.sim.owner == token
    assert not commands.admit(envelope(clock, "b" * 48))
    assert commands.owner == "b" * 48


@pytest.mark.parametrize("action", ["reset", "pause", "release"])
def test_reset_pause_and_release_do_not_let_existing_apps_reclaim_control(automatic_bus, action):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    commands.admit(envelope(clock))
    token = sim.owner
    commands.revoke()
    if action == "reset":
        sim.reset()
    elif action == "pause":
        sim.pause()
        sim.resume()
    else:
        sim.release(token)
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert sim.owner is None
    assert commands.status()["sources"][0]["requires_restart"]
    with pytest.raises(PermissionError, match="restart"):
        commands.grant("a" * 48)
    assert not commands.admit(envelope(clock, "b" * 48))
    assert commands.owner == "b" * 48


def test_new_source_seen_during_pause_cannot_auto_start_on_resume(automatic_bus):
    commands, clock = automatic_bus
    sim = commands.runtime.sim
    commands.admit(envelope(clock))
    commands.revoke()
    sim.pause()
    clock.tick()
    assert not commands.admit(envelope(clock, "b" * 48))
    sim.resume()
    clock.tick()
    assert not commands.admit(envelope(clock))
    assert not commands.admit(envelope(clock, "b" * 48))
    assert sim.owner is None
    assert not commands.admit(envelope(clock, "c" * 48))
    assert commands.owner == "c" * 48


@pytest.mark.parametrize("kind", ["sport", "lowcmd", "motion_switcher"])
def test_native_publishers_still_require_explicit_grants(automatic_bus, kind):
    commands, clock = automatic_bus
    seen_ownership = []
    def native(envelope, *, owned):
        seen_ownership.append(owned)
        return owned
    commands.native_handler = native
    assert not commands.admit(envelope(clock) | {"kind": kind})
    assert commands.runtime.sim.owner is None
    clock.tick()
    assert not commands.admit(envelope(clock) | {"kind": kind})
    assert seen_ownership == [False, False]
    if kind == "motion_switcher":
        with pytest.raises(ValueError, match="outside this profile"):
            commands.grant("a" * 48)
    else:
        commands.grant("a" * 48)
        clock.tick()
        assert commands.admit(envelope(clock) | {"kind": kind})


@pytest.mark.parametrize("first_kind,changed_kind", [("twist", "sport"), ("sport", "twist")])
def test_publisher_cannot_change_command_kind_under_its_grant(automatic_bus, first_kind, changed_kind):
    commands, clock = automatic_bus
    commands.native_handler = lambda envelope, owned: owned
    assert not commands.admit(envelope(clock) | {"kind": first_kind})
    if first_kind != "twist":
        commands.grant("a" * 48)
    token = commands.runtime.sim.owner
    clock.tick()
    with pytest.raises(ValueError, match="kind changed"):
        commands.admit(envelope(clock) | {"kind": changed_kind})
    assert commands.sources["a" * 48]["kind"] == first_kind
    assert commands.runtime.sim.owner == token
    assert commands.admit(envelope(clock) | {"kind": first_kind})


def test_native_zero_move_handoff_obeys_epoch_and_publisher_fences(automatic_bus):
    commands, clock = automatic_bus
    commands.native_auto_grant = lambda message: message.get('initial_zero') is True
    commands.native_handler = lambda message, owned: owned
    packet = envelope(clock) | {'kind': 'sport', 'initial_zero': True}
    assert not commands.admit(packet)
    assert commands.owner == 'a' * 48
    clock.tick()
    assert commands.admit(envelope(clock) | {'kind': 'sport'})
    clock.tick()
    assert not commands.admit(envelope(clock, 'b'*48) | {'kind': 'sport', 'initial_zero': True})
    assert commands.owner == 'b' * 48
    clock.tick()
    assert not commands.admit(envelope(clock) | {'kind': 'sport', 'initial_zero': True})
    commands.revoke()
    commands.runtime.sim.reset()
    clock.tick()
    assert not commands.admit(envelope(clock, 'b'*48) | {'kind': 'sport', 'initial_zero': True})
    assert commands.owner is None
