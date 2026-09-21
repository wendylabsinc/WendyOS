"""Command admission and ROS camera conventions, runnable without ROS installed."""
from types import SimpleNamespace

import numpy as np
import pytest

from coke_demo.ros_bridge import ROSBridge, camera_intrinsics, rotation_quaternion, validate_target


NAMES = [f"joint_{i}" for i in range(43)]


def command(positions=None, names=None, **point_overrides):
    point = dict(positions=list(np.zeros(43) if positions is None else positions),
                 velocities=[], accelerations=[], effort=[],
                 time_from_start=SimpleNamespace(sec=0, nanosec=25_000_000))
    point.update(point_overrides)
    return SimpleNamespace(joint_names=NAMES if names is None else names,
                           points=[SimpleNamespace(**point)])


def bridge():
    instance = ROSBridge(NAMES, joint_bounds=np.tile([-1., 1.], (43, 1)))
    instance.node = SimpleNamespace(get_logger=lambda: SimpleNamespace(warning=lambda message: None))
    return instance


def test_named_positions_reorder_into_model_order():
    actual = validate_target(NAMES[::-1], np.arange(43)[::-1], NAMES)
    np.testing.assert_array_equal(actual, np.arange(43))


@pytest.mark.parametrize("names,values", [
    (NAMES[:-1], np.zeros(42)),
    (NAMES[:-1] + [NAMES[0]], np.zeros(43)),
    (NAMES[:-1] + ["unknown"], np.zeros(43)),
    (NAMES, np.zeros(42)),
    (NAMES, [float("nan")] + [0.] * 42),
    (NAMES, [float("inf")] + [0.] * 42),
])
def test_bad_joint_contract_is_rejected(names, values):
    with pytest.raises(ValueError):
        validate_target(names, values, NAMES)


def test_commands_are_bounded_latest_only_and_reject_unsupported_controls():
    instance = bridge()
    instance._receive_command(command(np.full(43, .1)))
    instance._receive_command(command(np.full(43, .2)))
    np.testing.assert_array_equal(instance.take_command(), np.full(43, .2))
    assert instance.take_command() is None
    instance._receive_command(command(np.full(43, 2.)))
    instance._receive_command(command(velocities=[0.] * 43))
    instance._receive_command(command(time_from_start=SimpleNamespace(sec=2, nanosec=0)))
    msg = command()
    msg.points *= 2
    instance._receive_command(msg)
    assert instance.take_command() is None
    assert instance.status()["counts"]["rejected"] == 4


def test_expired_command_and_reset_flush(monkeypatch):
    instance = bridge()
    monkeypatch.setattr("coke_demo.ros_bridge.time.monotonic", lambda: 1.)
    instance._receive_command(command())
    monkeypatch.setattr("coke_demo.ros_bridge.time.monotonic", lambda: 1.6)
    assert instance.take_command() is None
    instance._receive_command(command())
    instance.clear_commands()
    assert instance.take_command() is None


def test_camera_calibration_and_optical_axis_conversion():
    intrinsics = np.asarray(camera_intrinsics(320, 240, 90)).reshape(3, 3)
    np.testing.assert_allclose(intrinsics, [[120, 0, 159.5], [0, 120, 119.5], [0, 0, 1]])
    np.testing.assert_allclose(rotation_quaternion(np.eye(3)), [0, 0, 0, 1])
    # A MuJoCo camera with identity world orientation looks towards world -Z.
    np.testing.assert_allclose(rotation_quaternion(np.diag([1, -1, -1])), [1, 0, 0, 0])
    np.testing.assert_allclose(rotation_quaternion(np.diag([-1, 1, -1])), [0, 1, 0, 0])
    np.testing.assert_allclose(rotation_quaternion(np.diag([-1, -1, 1])), [0, 0, 1, 0])


def test_idle_heartbeat_replays_capture_timestamps_without_changing_live_cadence(monkeypatch):
    instance = bridge()
    topics = ["simulation/epoch", "joint_states", "camera/color/image_raw", "camera/depth/image_rect_raw",
              "perception/can_mask", "camera/color/camera_info", "camera/depth/camera_info", "perception/can_visible"]
    messages = {name: [] for name in topics}
    instance._publishers = {name: SimpleNamespace(publish=output.append) for name, output in messages.items()}
    clocks, transforms = [], []
    instance._clock = SimpleNamespace(publish=clocks.append)
    instance._tf = SimpleNamespace(sendTransform=transforms.append)
    simple_message = lambda: SimpleNamespace(header=SimpleNamespace())
    transform_message = lambda: SimpleNamespace(header=SimpleNamespace(), transform=SimpleNamespace(
        translation=SimpleNamespace(), rotation=SimpleNamespace()))
    instance._types = {name: SimpleNamespace for name in ["Time", "Clock", "Bool", "UInt64"]}
    instance._types.update({name: simple_message for name in ["JointState", "Image", "CameraInfo"]})
    instance._types["TransformStamped"] = transform_message
    wall = [10.]
    monkeypatch.setattr("coke_demo.ros_bridge.time.monotonic", lambda: wall[0])
    obs = dict(epoch=1, q43=np.zeros(43), dq43=np.zeros(43), rgb_u8=np.zeros((3, 4, 3), dtype=np.uint8),
               depth_m=np.ones((3, 4)), mask=np.ones((3, 4), dtype=bool), detection_valid=True,
               camera_position=[0, 0, 1], camera_rotation=np.eye(3))
    for frame in range(40):
        wall[0] = 10 + frame * .025
        obs.update(frame=frame, sim_time=frame * .025, camera_frame=frame // 2 * 2,
                   camera_sim_time=frame // 2 * .05)
        instance.publish(obs)
        wall[0] += .005
        instance.publish(obs)
    assert len(messages["joint_states"]) == 40
    assert len(messages["camera/color/image_raw"]) == 20
    assert instance.status()["counts"]["heartbeats"] == 0
    # Same frame identity with changed caller memory must still replay the
    # already serialized exposure and its original timestamps.
    obs["rgb_u8"][:] = 255
    obs.update(sim_time=100., camera_sim_time=200.)
    wall[0] = 10 + 39 * .025 + .999
    instance.publish(obs)
    assert len(clocks) == 40
    wall[0] += .002
    instance.publish(obs)
    assert len(clocks) == 41
    assert clocks[-1].clock.sec == 0 and clocks[-1].clock.nanosec == 975_000_000
    assert messages["joint_states"][-1].header.stamp.nanosec == 975_000_000
    camera = messages["camera/color/image_raw"][-1]
    assert camera.header.stamp.nanosec == 950_000_000
    assert camera.data == bytes(3 * 4 * 3)
    assert transforms[-1][-1].header.stamp.nanosec == 950_000_000
    assert instance.status()["counts"]["observations"] == 40
    assert instance.status()["counts"]["camera_frames"] == 20
    assert instance.status()["counts"]["heartbeats"] == 1
    # Frequent idle calls cannot raise the heartbeat above 1 Hz.
    for _ in range(5):
        wall[0] += .1
        instance.publish(obs)
    assert len(clocks) == 41
