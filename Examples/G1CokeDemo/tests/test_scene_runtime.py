"""Pinned recorded expert through actual MuJoCo; no policy inference or robot."""
import os
from pathlib import Path

import mujoco
import numpy as np
import pytest

from coke_demo.scene import CokeScene
from coke_demo.simulation import Simulation


pytestmark = pytest.mark.skipif('WENDY_COKE_EXPERT' not in os.environ, reason='use required exact-asset scene runtime gate')

def test_recorded_scene_default_replay_reset_and_real_observation():
    # The required wrapper verifies every original expert hash before imports.
    # Absent inputs are a failure in this dedicated gate, never a pytest skip.
    expert = Path(os.environ['WENDY_COKE_EXPERT'])
    scene = CokeScene(expert)
    replay = Simulation(expert)
    try:
        assert scene.epoch == 1, 'COKE_INITIAL_EPOCH wrong first scene reset dispatch'
        assert scene.frame == 0 and scene._camera is None
        assert (scene.model.nq, scene.model.nv, scene.model.nu) == (50, 49, 43)
        assert len(scene.references) == 2400
        before = scene.data.qpos.copy()
        initial = scene.observation()
        np.testing.assert_array_equal(scene.data.qpos, before)
        assert initial['epoch'] == 1 and initial['frame'] == initial['camera_frame'] == 0
        assert 'joint_effort43' in initial and 'joint_effort' not in initial, 'COKE_EFFORT_SCHEMA wrong 43-joint observation field'
        np.testing.assert_array_equal(initial['joint_effort43'], scene._pose_data.qfrc_actuator[scene.qv])
        assert initial['joint_effort43'].shape == (43,) and np.isfinite(initial['joint_effort43']).all()
        assert initial['image'].shape == (1, 5, 240, 320) and np.isfinite(initial['image']).all()
        assert initial['rgb_u8'].shape == (240, 320, 3) and np.ptp(initial['rgb_u8']) > 0
        for frame in range(2400):
            try:
                advanced = scene.step()
            except TypeError as error:
                raise AssertionError('COKE_DEFAULT_REFERENCE missing actual scene default stepping') from error
            assert advanced is True and replay.step() is True
            np.testing.assert_array_equal(scene.data.qpos, replay.data.qpos)
            np.testing.assert_array_equal(scene.data.qvel, replay.data.qvel)
            assert scene.frame == replay.frame == frame + 1
        stopped = scene.data.qpos.copy()
        assert scene.step() is False and replay.step() is False
        np.testing.assert_array_equal(scene.data.qpos, stopped)
        final = scene.observation()
        assert final['camera_frame'] == 2400 and final['camera_sim_time'] == 60.0
        assert final['camera_captured_at_ns'] > initial['camera_captured_at_ns']
        assert not np.array_equal(final['rgb_u8'], initial['rgb_u8'])
        metrics = scene.metrics()
        assert metrics['physical_commands_sent'] == 0 and metrics['duration_seconds'] == 60
        assert metrics['maximum_lift_m'] > .25, 'recorded expert did not lift the simulated can'
        scene.reset()
        reset = scene.observation()
        assert reset['epoch'] == 2 and reset['frame'] == reset['camera_frame'] == 0
        np.testing.assert_array_equal(scene.data.qpos, before)
        assert reset['camera_captured_at_ns'] > final['camera_captured_at_ns']
        print(f'COKE_SCENE_RUNTIME_VERIFIED MuJoCo={mujoco.__version__} steps=2400 epoch=1/2 real RGBD joint_effort43 metrics={metrics}')
    finally:
        scene.close()
