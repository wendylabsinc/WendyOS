"""Actual integration-state capture, bounded retention, and CRC conformance."""

import random
import struct
from types import SimpleNamespace

import numpy as np

from g1_sim.observations import SnapshotQueue, load_snapshot
from g1_sim.sensors import PhysicsSampler, instrumented_model
from g1_sim.simulation import DEFAULT_ASSETS, Simulation
from g1_sim.unitree_crc import crc_words


def fixture(capacity=10, max_age_ns=20_000_000):
    clock = SimpleNamespace(monotonic=100_000_000, wall=123_000_000_000)
    sim = Simulation(model=instrumented_model(DEFAULT_ASSETS))
    runtime = SimpleNamespace(sim=sim, observation_generation=4)
    queue = SnapshotQueue(sim, capacity=capacity, max_age_ns=max_age_ns,
                          monotonic_ns=lambda: clock.monotonic, wall_ns=lambda: clock.wall)
    return runtime, queue, clock


def test_each_genuine_step_is_consumed_once_with_original_capture_time():
    runtime, queue, clock = fixture()
    poses = []
    for _ in range(5):
        runtime.sim.step()
        poses.append(runtime.sim.data.qpos.copy())
        queue.capture(runtime)
        clock.monotonic += 2_000_000
        clock.wall += 2_000_000
    sampler = PhysicsSampler(runtime.sim)
    for index in range(5):
        snapshot = queue.take()
        load_snapshot(sampler, snapshot)
        state = sampler.state()
        assert state["wall_timestamp_ns"] == 123_000_000_000 + index * 2_000_000
        assert np.isclose(state["time"], (index + 1) * 0.002)
        assert snapshot.generation == 4
        np.testing.assert_array_equal(sampler.data.qpos, poses[index])
    assert queue.take() is None
    assert queue.status()["consumed"] == 5


def test_queue_drops_oldest_overflow_and_expired_states_without_restamping():
    runtime, queue, clock = fixture(capacity=2)
    for _ in range(3):
        runtime.sim.step()
        queue.capture(runtime)
        clock.monotonic += 2_000_000
        clock.wall += 2_000_000
    assert queue.status()["overflow"] == 1
    snapshot = queue.take()
    assert snapshot.wall_timestamp_ns == 123_002_000_000
    clock.monotonic += 21_000_000
    assert queue.take() is None
    assert queue.status()["expired"] == 1
    queue.capture(runtime)
    clock.monotonic -= 1
    assert queue.take() is None
    assert queue.status()["expired"] == 2


def reference_crc(data):
    # Independent bit-by-bit Unitree algorithm, no reflected CRC or lookup table.
    crc = 0xFFFFFFFF
    for word, in struct.iter_unpack("<I", data):
        for bit in range(31, -1, -1):
            top = (crc >> 31) ^ ((word >> bit) & 1)
            crc = (crc << 1) & 0xFFFFFFFF
            if top:
                crc ^= 0x04C11DB7
    return crc


def test_accelerated_crc_matches_native_word_bitstream_for_arbitrary_records():
    rng = random.Random(42)
    for count in (0, 1, 2, 202, 294, 1024):
        record = b"".join(struct.pack("<I", rng.getrandbits(32)) for _ in range(count))
        assert crc_words(record) == reference_crc(record)
    assert crc_words(bytes(1180 - 4)) == reference_crc(bytes(1180 - 4))
