from io import BytesIO

from PIL import Image
import pytest

from wendy_app.state import CORE, Observations, camera_jpeg


@pytest.fixture
def telemetry():
    now = [100.0]
    observations = Observations(clock=lambda: now[0], wall_clock=lambda: now[0])
    observations.set_simulator({"simulation": True, "robot_kind": "g1", "healthy": True})
    for key in CORE:
        observations.record(key, {"names": ["joint"]} if key == "joints" else {}, 100)
    return observations, now


def test_live_data_then_stale_does_not_stay_ready(telemetry):
    observations, now = telemetry
    assert observations.snapshot()["ready"]
    now[0] += 2.1
    assert not observations.snapshot()["ready"]
    assert not observations.snapshot()["topics"]["camera"]["fresh"]


def test_recent_delivery_of_old_source_message_is_not_ready(telemetry):
    observations, _ = telemetry
    observations.record("joints", {}, 90)
    assert not observations.snapshot()["ready"]


def test_wrong_simulator_and_sensor_failure_are_not_ready(telemetry):
    observations, _ = telemetry
    observations.failure("imu", "nonfinite data")
    assert not observations.snapshot()["ready"]
    observations.record("imu", {}, 100)
    assert observations.snapshot()["ready"]
    observations.set_simulator({"simulation": False, "robot_kind": "g1", "healthy": True})
    assert not observations.snapshot()["ready"]


def test_readiness_never_claims_grasp_policy_compatibility(telemetry):
    observations, _ = telemetry
    policy = observations.snapshot()["policy"]
    assert not policy["compatible"] and not policy["running"]
    assert policy["observed_joint_count"] == 1 and policy["required_joint_count"] == 43


def test_padded_bgr_image_and_invalid_payload():
    frame = (2, 2, "bgr8", 8, bytes([0, 0, 255] * 2 + [0, 0]) * 2)
    image = Image.open(BytesIO(camera_jpeg(frame)))
    assert image.size == (2, 2)
    r, g, b = image.getpixel((0, 1))
    assert r > 240 and g < 10 and b < 10
    with pytest.raises(ValueError):
        camera_jpeg((2, 2, "rgb8", 6, b"too short"))


def test_stale_camera_is_not_served(telemetry):
    observations, now = telemetry
    frame = (1, 1, "rgb8", 3, bytes([255, 0, 0]))
    observations.record("camera", {}, 100, frame)
    assert observations.jpeg().startswith(b"\xff\xd8")
    now[0] += 3
    assert observations.jpeg() is None


def test_frequency_decays_after_messages_stop(telemetry):
    observations, now = telemetry
    now[0] += 0.05
    observations.record("joints", {}, now[0])
    assert observations.snapshot()["topics"]["joints"]["hz"] == 20
    now[0] += 4
    assert observations.snapshot()["topics"]["joints"]["hz"] == 0
