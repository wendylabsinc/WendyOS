"""Control admission, HTTP and publisher checks; no ROS installation needed."""

from concurrent.futures import ThreadPoolExecutor
from http.server import ThreadingHTTPServer
import json
import sys
import threading
from types import SimpleNamespace
from urllib.error import HTTPError
from urllib.request import Request, urlopen

import pytest

from teleop import (ControlError, DEADMAN_SECONDS, PUBLISH_SECONDS, TeleopControl,
                    ZERO, handler_for, make_node, parse_json, velocity)


def request(session="test", sequence=1, keys=None, speed=0.3, turn_speed=0.6):
    return {"session": session, "sequence": sequence, "keys": ["w"] if keys is None else keys,
            "speed": speed, "turn_speed": turn_speed}


@pytest.fixture
def rig():
    clock, commands = [10.0], []
    control = TeleopControl(lambda *values: commands.append(values), lambda: clock[0])
    return control, clock, commands


def test_startup_idle_and_release_publish_zero_immediately(rig):
    control, clock, commands = rig
    assert commands == [ZERO]
    control.tick()
    assert commands[-1] == ZERO
    session = control.enable({})["session"]
    control.drive(request(session))
    control.tick()
    assert commands[-1] == (0.3, 0.0, 0.0)
    control.release({"session": session, "sequence": 2})
    assert commands[-1] == ZERO
    assert not control.status()["enabled"]


def test_key_release_immediately_publishes_zero(rig):
    control, _, commands = rig
    session = control.enable({})["session"]
    control.drive(request(session))
    control.tick()
    control.drive(request(session, sequence=2, keys=[]))
    assert commands[-1] == ZERO


def test_deadman_expiry_is_terminal_even_before_next_ros_tick(rig):
    control, clock, commands = rig
    session = control.enable({})["session"]
    control.drive(request(session))
    clock[0] += DEADMAN_SECONDS
    with pytest.raises(ControlError, match="expired"):
        control.drive(request(session, sequence=2))
    assert commands[-1] == ZERO
    assert not control.status()["enabled"]


def test_periodic_publisher_stops_on_missing_heartbeat(rig):
    control, clock, commands = rig
    session = control.enable({})["session"]
    control.drive(request(session))
    clock[0] += DEADMAN_SECONDS - 0.01
    control.tick()
    assert commands[-1][0] > 0
    clock[0] += 0.01
    control.tick()
    assert commands[-1] == ZERO
    assert "expired" in control.status()["reason"]


def test_duplicate_and_reordered_drive_cannot_undo_key_release(rig):
    control, _, commands = rig
    session = control.enable({})["session"]
    control.drive(request(session, sequence=3, keys=[]))
    for sequence in (2, 3):
        with pytest.raises(ControlError, match="Out-of-order"):
            control.drive(request(session, sequence=sequence))
    control.tick()
    assert commands[-1] == ZERO


def test_old_tab_cannot_drive_or_release_new_session(rig):
    control, clock, commands = rig
    old = control.enable({})["session"]
    with pytest.raises(ControlError, match="Another tab"):
        control.enable({})
    clock[0] += DEADMAN_SECONDS
    new = control.enable({})["session"]
    assert old != new
    control.drive(request(new))
    with pytest.raises(ControlError):
        control.drive(request(old, sequence=50))
    with pytest.raises(ControlError):
        control.release({"session": old, "sequence": 51})
    control.tick()
    assert commands[-1][0] > 0


def test_competing_tabs_get_exactly_one_session(rig):
    control, _, _ = rig
    barrier = threading.Barrier(2)

    def enable():
        barrier.wait(timeout=2)
        try:
            return control.enable({})
        except ControlError as error:
            return error.status

    with ThreadPoolExecutor(max_workers=2) as pool:
        first, second = pool.submit(enable), pool.submit(enable)
        results = [first.result(timeout=2), second.result(timeout=2)]
    assert sum(isinstance(value, dict) for value in results) == 1
    assert 409 in results


def test_concurrent_timer_cannot_publish_nonzero_after_release():
    commands, publishing, unblock, released = [], threading.Event(), threading.Event(), threading.Event()

    def publish(*values):
        if values != ZERO:
            publishing.set()
            assert unblock.wait(timeout=2)
        commands.append(values)

    control = TeleopControl(publish, clock=lambda: 10.0)
    session = control.enable({})["session"]
    control.drive(request(session))

    def release():
        control.release({"session": session, "sequence": 2})
        released.set()

    with ThreadPoolExecutor(max_workers=2) as pool:
        tick = pool.submit(control.tick)
        assert publishing.wait(timeout=2)
        stop = pool.submit(release)
        try:
            assert not released.wait(timeout=0.02)
        finally:
            unblock.set()
        tick.result(timeout=2)
        stop.result(timeout=2)
    assert commands[-2:] == [(0.3, 0.0, 0.0), ZERO]


def test_shutdown_prevents_new_sessions(rig):
    control, _, commands = rig
    control.stop()
    with pytest.raises(ControlError, match="shutting down"):
        control.enable({})
    assert commands[-1] == ZERO


def test_velocity_caps_diagonals_and_cancels_opposite_keys():
    x, y, yaw = velocity(request(keys=["w", "a", "q"], speed=99, turn_speed=99))
    assert (x*x + y*y)**0.5 == pytest.approx(0.6)
    assert 0 < y <= 0.4
    assert yaw == 1.0
    assert velocity(request(keys=["w", "s", "a", "d", "q", "e"])) == ZERO
    assert velocity(request(speed=-1, turn_speed=-1, keys=["s", "e"])) == ZERO
    assert velocity(request(speed=10**400, keys=["s", "d", "e"]))[0] < 0


@pytest.mark.parametrize("change", [
    {"speed": float("nan")}, {"speed": float("inf")}, {"turn_speed": float("-inf")},
    {"speed": True}, {"speed": "0.3"}, {"keys": "w"}, {"keys": ["w", "w"]},
    {"keys": ["x"]}, {"keys": [["w"]]}, {"keys": [True]}, {"sequence": True},
    {"sequence": -1}, {"sequence": 2**53}, {"session": None}, {"surprise": "field"},
])
def test_malformed_drives_are_rejected_without_changing_state(rig, change):
    control, _, _ = rig
    session = control.enable({})["session"]
    before = control.status()
    with pytest.raises(ControlError):
        control.drive(request(session) | change)
    assert control.status() == before


@pytest.mark.parametrize("body", [b'{"x": 1, "x": 2}', b'{"x": NaN}', b'{"x": Infinity}', b'\xff', b'{'])
def test_invalid_json_is_rejected(body):
    with pytest.raises(ControlError):
        parse_json(body)


@pytest.fixture
def http(rig):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(rig[0]))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def call(path, body=None, headers=None):
        req = Request(f"http://127.0.0.1:{server.server_port}" + path, data=body,
                      headers={"Content-Type": "application/json"} | (headers or {}))
        try:
            response = urlopen(req, timeout=2)
        except HTTPError as error:
            response = error
        with response:
            data = response.read()
            return response.status, data, response.headers

    yield call
    server.shutdown()
    server.server_close()
    thread.join(timeout=2)


def test_http_page_and_control_round_trip(http, rig):
    status, body, headers = http("/")
    assert status == 200 and b"Take it for a walk" in body
    assert headers["Cache-Control"] == "no-store"
    status, body, _ = http("/api/enable", b"{}")
    assert status == 200
    session = json.loads(body)["session"]
    assert http("/api/drive", json.dumps(request(session)).encode())[0] == 200
    rig[0].tick()
    assert rig[2][-1][0] == 0.3
    assert http("/api/release", json.dumps({"session": session, "sequence": 2}).encode())[0] == 200
    assert rig[2][-1] == ZERO
    assert http("/api/drive", json.dumps(request(session, sequence=3)).encode())[0] == 409


def test_http_rejects_cross_origin_wrong_content_type_and_oversize(http):
    assert http("/api/enable", b"{}", {"Origin": "https://other.example"})[0] == 403
    assert http("/api/enable", b"{}", {"Content-Type": "text/plain"})[0] == 415
    assert http("/api/enable", b"[]")[0] == 400
    assert http("/api/enable", b"{" + b" " * 4096 + b"}")[0] == 413
    assert http("/api/drive", b'{"speed": NaN}')[0] == 400
    assert http("/missing", b"{}")[0] == 404


def test_ros_adapter_starts_zero_and_publishes_sport_at_20_hz(monkeypatch):
    commands, timers, publishers = [], [], []

    class FakeNode:
        def __init__(self, name):
            self.name = name

        def create_publisher(self, kind, topic, qos):
            publishers.append((kind, topic, qos.depth))
            return SimpleNamespace(publish=commands.append)

        def create_timer(self, period, callback):
            timers.append((period, callback))

    class Twist:
        def __init__(self):
            self.header = SimpleNamespace(identity=SimpleNamespace(id=0, api_id=0),
                                          policy=SimpleNamespace(noreply=False))
            self.parameter = ""

    for name, module in {
        "rclpy.node": SimpleNamespace(Node=FakeNode),
        "rclpy.qos": SimpleNamespace(QoSProfile=lambda **kwargs: SimpleNamespace(**kwargs)),
        "unitree_api.msg": SimpleNamespace(Request=Twist),
    }.items():
        monkeypatch.setitem(sys.modules, name, module)
    node = make_node()
    assert publishers == [(Twist, "/api/sport/request", 1)]
    assert json.loads(commands[0].parameter) == {"x": 0.0, "y": 0.0, "z": 0.0}
    assert commands[0].header.identity.api_id == 1008
    assert timers[0][0] == PUBLISH_SECONDS == 0.05
    session = node.control.enable({})["session"]
    node.control.drive(request(session, keys=["a", "e"]))
    timers[0][1]()
    assert json.loads(commands[-1].parameter) == {"x": 0.0, "y": 0.3, "z": -0.6}
