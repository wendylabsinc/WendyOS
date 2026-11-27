#!/usr/bin/env python3
"""Seven raw observation sources from one stepped office/people MuJoCo scene.

The existing hazard source owns the clock, capture cache and HTTP controls.
Camera endpoint aliases read that same cache; detector inputs contain pixels and
calibration, never actor positions or inferred hazards/tracks.
"""
from integration.hazard.scene import OfficeScene, Source as HazardSource, SENSORS as HAZARD_SENSORS
from integration.hazard.scene import encoded, main as source_main, server as stepped_server
from sim.shared_inspection import ContinuousSource as InspectionSource

SENSORS = {**HAZARD_SENSORS, 'people-front': 'camera', 'people-oblique': 'camera'}
SCENARIO = 'shared-office-perception-v1'
CAMERAS = {'camera-a': 'camera-a', 'camera-b': 'corridor-view',
           'people-front': 'people-front', 'people-oblique': 'people-oblique'}


def _scene(run_id):
    from sim.shared_perception_scene import SharedPerceptionScene
    return SharedPerceptionScene(run_id)


class SharedOfficeScene(OfficeScene):
    """Reuse the five raw sensor generators on the shared compiled scene."""
    sensor_kinds = SENSORS
    scenario_version = SCENARIO
    calibration_revision = 'shared-office-cameras-v1'

    def __init__(self, run_id, capture_resolution_ns=1, *, inspection=False):
        if capture_resolution_ns not in (1, 1000):
            raise ValueError('nanosecond or SensorLink microsecond capture resolution required')
        if inspection:
            from sim.shared_perception_scene import SharedPerceptionScene
            self.shared = SharedPerceptionScene(run_id, inspection=True)
        else:
            self.shared = _scene(run_id)
        self.run_id, self.capture_resolution_ns = run_id, capture_resolution_ns
        # The inherited LiDAR generator uses these exact objects, as do all four
        # camera render/calibration calls. There is no second model or clock.
        for name in ('model', 'data', 'mj', 'np', 'xml', 'model_sha256',
                     'package_sha256', 'frame', 'config', 'bindings'):
            setattr(self, name, getattr(self.shared, name))
        self.cameras = dict(CAMERAS)

    def advance(self, ns):
        self.shared.advance(ns)

    def render(self, camera_name):
        return self.shared.render(camera_name)

    def calibration(self, camera_name):
        return self.shared.calibration(camera_name)

    def presentation(self):
        return self.shared.presentation()

    def close(self):
        self.shared.close()


class Source(HazardSource):
    def __init__(self, run_id, anchor, renderer=None, capture_resolution_ns=1):
        super().__init__(run_id, anchor, renderer, capture_resolution_ns,
                         scene_factory=SharedOfficeScene)

    def capture_batches(self, sensors):
        # Publish completed hazard observations before the two larger people
        # renders. All captures still use the same serialized model/data and
        # sample instant; the final step receipt waits for both groups.
        hazard = [sensor for sensor in sensors if sensor in HAZARD_SENSORS]
        people = [sensor for sensor in sensors if sensor not in HAZARD_SENSORS]
        return (hazard, people) if people else (hazard,)


class ContinuousSource(InspectionSource):
    """Construct the opt-in runtime; ordinary Source remains stepped."""
    def __init__(self, run_id, anchor, renderer=None, capture_resolution_ns=1, *, defer_start=False, fixed_capture_hz=5):
        super().__init__(run_id, anchor, renderer, capture_resolution_ns,
            scene_factory=lambda run, resolution: SharedOfficeScene(run, resolution, inspection=True),
            defer_start=defer_start, fixed_capture_hz=fixed_capture_hz)


def server(source, address, read_token, control_token):
    from sim.shared_inspection import inspection_server
    return inspection_server(source, stepped_server(source, address, read_token, control_token),
                             read_token, control_token)


def main():
    import sys
    if '--inspection' not in sys.argv:
        return source_main(Source)
    from sim.shared_inspection import serve_inspection
    serve_inspection(ContinuousSource, server)


if __name__ == '__main__':
    main()
