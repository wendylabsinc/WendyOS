"""Pixel, geometry and freshness checks; generated test images never enter Git."""
import copy
import hashlib
import io
from pathlib import Path
import sys
import unittest

from PIL import Image, ImageDraw

sys.path.insert(0, str(Path(__file__).resolve().parent))
from perception import Detector, floor_point, simulated_pinhole_contract, validate_calibration


def scene(kind="flame"):
    image = Image.new("RGB", (640, 480), (35, 45, 55))
    draw = ImageDraw.Draw(image)
    if kind == "flame":
        draw.polygon([(320, 120), (260, 400), (380, 400)], fill=(245, 70, 10))
        draw.polygon([(320, 230), (290, 390), (350, 390)], fill=(255, 220, 30))
    elif kind == "orange":
        draw.rectangle((260, 120, 380, 400), fill=(245, 105, 20))
    elif kind == "two-color-box":
        draw.rectangle((260, 120, 380, 400), fill=(245, 70, 10))
        draw.rectangle((290, 230, 350, 390), fill=(255, 220, 30))
    elif kind == "yellow":
        draw.polygon([(320, 120), (260, 400), (380, 400)], fill=(255, 220, 30))
    encoded = io.BytesIO()
    image.save(encoded, format="JPEG", quality=85)
    return encoded.getvalue()


def row(blob, sequence=0, session="boot-a"):
    return {"session_id": session, "channel_id": 0, "frame_seq": sequence,
            "timestamp_us": (sequence+1)*200000, "width": 640, "height": 480,
            "sha256": hashlib.sha256(blob).hexdigest()}


def calibration():
    return {"schema": "wendy.xiao-fire-camera.v1", "origin": "simulated", "run_id": "run-a",
            **simulated_pinhole_contract(640, 480),
            "width": 640, "height": 480, "fx": 400., "fy": 400., "cx": 320., "cy": 240.,
            "camera_to_world": [1, 0, 0, 0, 0, 0, -1, 3, 0, 1, 0, 0, 0, 0, 0, 1],
            "floor_y_m": 0., "revision": "synthetic-intrinsics-1", "frame_id": "office-floor",
            "frame_revision": "sim-office-1"}


class PerceptionTests(unittest.TestCase):
    def setUp(self):
        self.blob, self.camera = scene(), calibration()

    def observe(self, detector, sequence, received=None, now=None, blob=None, session="boot-a"):
        received = 10+sequence*.2 if received is None else received
        now = received+.01 if now is None else now
        blob = self.blob if blob is None else blob
        return detector.detect(blob, row(blob, sequence, session), self.camera, received, now)

    def test_three_pixel_detections_bind_actual_frame_metadata_and_floor_point(self):
        detector = Detector("run-a")
        first = self.observe(detector, 0)
        self.assertTrue(first["candidate"])
        self.assertFalse(first["detected"])
        self.assertFalse(self.observe(detector, 1)["detected"])
        final = self.observe(detector, 2)
        self.assertTrue(final["detected"])
        self.assertEqual(final["origin"], "inferred")
        self.assertEqual(final["score_kind"], "fixture_rule_not_probability")
        self.assertEqual([i["frame_seq"] for i in final["confirming_inputs"]], [0, 1, 2])
        self.assertEqual(final["source"]["sha256"], hashlib.sha256(self.blob).hexdigest())
        self.assertEqual(final["source"]["timestamp_us"], 600000)
        self.assertEqual(final["expires_monotonic_s"], 12.)
        self.assertEqual(final["source"]["simulation_run"], "run-a")
        self.assertAlmostEqual(final["position_m"][0], 0., delta=.02)
        self.assertEqual(final["position_m"][1], 0.)
        self.assertAlmostEqual(final["position_m"][2], 1.2, delta=.04)

    def test_clean_or_orange_objects_do_not_generate_a_fire_claim(self):
        for kind in ("clean", "orange", "two-color-box", "yellow"):
            with self.subTest(kind=kind):
                detector = Detector("run-a")
                for sequence in range(4):
                    result = self.observe(detector, sequence, blob=scene(kind))
                    self.assertFalse(result["detected"])
                    self.assertFalse(result["candidate"])

    def test_repeated_frame_does_not_confirm_or_extend_original_deadline(self):
        detector = Detector("run-a")
        self.observe(detector, 0)
        for now in (10.2, 10.4, 11.9):
            result = self.observe(detector, 0, received=10, now=now)
            self.assertFalse(result["detected"])
            self.assertEqual(result["reason"], "repeated_or_out_of_order_frame")
        self.assertFalse(self.observe(detector, 1, received=11.95, now=11.96)["detected"])
        final = self.observe(detector, 2, received=12.1, now=12.11)
        self.assertFalse(final["detected"])
        self.assertEqual(final["confirmation_frames"], 2)

    def test_expired_frame_and_sequence_gap_reset_confirmation(self):
        detector = Detector("run-a")
        self.observe(detector, 0)
        self.observe(detector, 1)
        expired = self.observe(detector, 2, received=10.4, now=13)
        self.assertEqual(expired["reason"], "expired_frame")
        self.assertFalse(expired["detected"])
        self.assertEqual(self.observe(detector, 3, received=13.1)["confirmation_frames"], 1)
        self.assertEqual(self.observe(detector, 5, received=13.2)["confirmation_frames"], 1)

    def test_clean_frame_resets_a_pending_candidate(self):
        detector = Detector("run-a")
        self.observe(detector, 0)
        self.observe(detector, 1)
        self.observe(detector, 2, blob=scene("clean"))
        self.assertEqual(self.observe(detector, 3)["confirmation_frames"], 1)

    def test_exact_expiry_and_mixed_channels_cannot_confirm(self):
        detector = Detector("run-a")
        self.observe(detector, 0)
        self.observe(detector, 1)
        metadata = row(self.blob, 2)
        metadata["channel_id"] = 1
        with self.assertRaises(ValueError):
            detector.detect(self.blob, metadata, self.camera, 10.4, 10.41)
        result = self.observe(detector, 2, received=10.4, now=12.4)
        self.assertFalse(result["detected"])
        self.assertEqual(result["reason"], "expired_frame")

    def test_restart_retires_old_session_and_requires_new_observations(self):
        detector = Detector("run-a")
        for i in range(3):
            self.observe(detector, i)
        newer = self.observe(detector, 0, session="boot-b", received=11)
        self.assertFalse(newer["detected"])
        self.assertEqual(newer["confirmation_frames"], 1)
        self.assertEqual(self.observe(detector, 3, received=11.1)["reason"], "retired_session")

    def test_corruption_dimensions_scope_and_bad_calibration_are_rejected(self):
        for change in (lambda r,c: r.update(sha256="0"*64),
                       lambda r,c: r.update(sha256=None),
                       lambda r,c: c.update(run_id="other"),
                       lambda r,c: c.update(origin="measured"),
                       lambda r,c: c.update(width=1280),
                       lambda r,c: c.update(fx=float("nan")),
                       lambda r,c: c["camera_to_world"].__setitem__(0, 2),
                       lambda r,c: r.update(width=True)):
            metadata, camera = row(self.blob), copy.deepcopy(self.camera)
            change(metadata, camera)
            with self.assertRaises(ValueError):
                Detector("run-a").detect(self.blob, metadata, camera, 10., 10.1)

    def test_projection_uses_declared_optical_axes_and_rejects_horizon(self):
        validate_calibration(self.camera, "run-a", (640, 480))
        self.assertEqual(floor_point([320, 240], self.camera), [0., 0., 0.])
        projected = floor_point([400, 240], self.camera)
        self.assertAlmostEqual(projected[0], .6)
        self.assertEqual(projected[1:], [0., 0.])
        camera = copy.deepcopy(self.camera)
        camera["camera_to_world"] = [1,0,0,0, 0,1,0,3, 0,0,1,0, 0,0,0,1]
        with self.assertRaises(ValueError):
            floor_point([320, 240], camera)

    def test_projection_rejects_other_lenses_modes_and_pixel_transforms(self):
        unsupported = {"projection_model": "fisheye", "distortion_model": "brown_conrady",
                       "distortion": [0., 0., 0., 0., 0.], "capture_mode": "H.264 640x480",
                       "image_crop": [10, 0, 630, 480], "rectified": True,
                       "calibration_provenance": "measured"}
        for key, value in unsupported.items():
            for missing in (False, True):
                with self.subTest(key=key, missing=missing):
                    camera = copy.deepcopy(self.camera)
                    if missing:
                        del camera[key]
                    else:
                        camera[key] = value
                    with self.assertRaisesRegex(ValueError, "camera contract"):
                        Detector("run-a").detect(self.blob, row(self.blob), camera, 10., 10.1)
        for mode in ("MJPEG 1280x960", "MJPEG 480x640"):
            camera = dict(self.camera, capture_mode=mode)
            with self.assertRaisesRegex(ValueError, "capture_mode"):
                validate_calibration(camera, "run-a", (640, 480))


if __name__ == "__main__":
    unittest.main()
