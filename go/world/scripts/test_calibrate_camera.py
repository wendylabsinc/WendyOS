import argparse
import hashlib
import importlib.util
import json
import itertools
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('camera_calibration', Path(__file__).with_name('calibrate-camera.py'))
calibration = importlib.util.module_from_spec(spec)
spec.loader.exec_module(calibration)


class CalibrationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def synthetic_views(self, noise=.04):
        cv2, np = calibration.libraries()
        rng = np.random.default_rng(19)
        matrix = np.array([[1040., 0., 632.], [0., 1030., 478.], [0., 0., 1.]])
        distortion = np.array([-.12, .035, .001, -.002, .004])
        obj = calibration.object_points(9, 6, 20)
        views = []
        for i in range(40):
            rvec = np.array([(-.6, -.2, .2, .6)[i % 4], (-.4, 0, .4)[i % 3], -.25 + .1 * (i % 6)])
            tvec = np.array([-.20 + .075 * (i % 5), -.15 + .07 * (i % 4), .5 + .07 * (i % 5)])
            pts, _ = cv2.projectPoints(obj, rvec, tvec, matrix, distortion)
            pts = pts.reshape(-1, 2) + rng.normal(0, noise, (54, 2))
            if pts[:, 0].min() < 4 or pts[:, 1].min() < 4 or pts[:, 0].max() > 1276 or pts[:, 1].max() > 956:
                continue
            if abs(cv2.contourArea(pts[[0, 8, 53, 45]].astype(np.float32))) / (1280*960) < .02:
                continue
            views.append({'file': f'{i:03d}.jpg', 'sha256': hashlib.sha256(str(i).encode()).hexdigest(), 'points': pts.astype(np.float32)})
        return views, matrix, distortion

    def test_recovers_intrinsics_with_independent_held_out_views(self):
        views, matrix, distortion = self.synthetic_views()
        result = calibration.fit_views(views, (1280, 960))
        self.assertGreaterEqual(result['training_views'], 12)
        self.assertGreaterEqual(result['validation_views'], 3)
        self.assertFalse(set(result['training_files']) & set(result['validation_files']))
        self.assertLess(abs(result['intrinsics']['fx'] - matrix[0, 0]), 5)
        self.assertLess(abs(result['intrinsics']['fy'] - matrix[1, 1]), 5)
        self.assertLess(abs(result['intrinsics']['cx'] - matrix[0, 2]), 5)
        self.assertLess(abs(result['intrinsics']['cy'] - matrix[1, 2]), 5)
        self.assertLess(result['validation_rms_pixels'], .2)
        self.assertGreater(result['diversity']['normal_span_degrees'], 15)

    def test_rejects_repeated_or_insufficient_views(self):
        views, _, _ = self.synthetic_views()
        with self.assertRaisesRegex(ValueError, '15 distinct'):
            calibration.fit_views(views[:14], (1280, 960))
        with self.assertRaisesRegex(ValueError, 'both image axes'):
            calibration.fit_views([views[0]] * 20, (1280, 960))

    def test_bad_held_out_views_do_not_change_fit_or_pass_validation(self):
        _, np = calibration.libraries()
        views, _, _ = self.synthetic_views()
        rng = np.random.default_rng(8)
        for i in range(4, len(views), 5):
            views[i]['points'] += rng.normal(0, 8, views[i]['points'].shape).astype(np.float32)
        with self.assertRaisesRegex(ValueError, 'held-out accuracy'):
            calibration.fit_views(views, (1280, 960))

    def test_low_tilt_is_rejected_even_with_coverage(self):
        cv2, np = calibration.libraries()
        obj = calibration.object_points(9, 6, 20)
        matrix = np.array([[1040., 0., 640.], [0., 1040., 480.], [0., 0., 1.]])
        views = []
        for i in range(25):
            # Tiny rather than identical tilts avoid the exact planar singularity.
            rvec = np.array([.001*(i%3), .001*(i%4), .01*(i%5)])
            tvec = np.array([-.18 + .065*(i%5), -.15 + .06*(i%4), .55 + .1*(i%4)])
            pts, _ = cv2.projectPoints(obj, rvec, tvec, matrix, np.zeros(5))
            views.append({'file': str(i), 'sha256': str(i), 'points': pts.reshape(-1, 2).astype(np.float32)})
        with self.assertRaises(ValueError):
            calibration.fit_views(views, (1280, 960))

    def test_fixed_lens_transfer_validation_never_refits(self):
        views, matrix, distortion = self.synthetic_views()
        intrinsics = {'fx': matrix[0, 0], 'fy': matrix[1, 1], 'cx': matrix[0, 2], 'cy': matrix[1, 2], 'distortion': distortion.tolist()}
        with patch.object(calibration.libraries()[0], 'calibrateCamera', side_effect=AssertionError('must not refit')):
            result = calibration.fixed_lens_validation(views, (1280, 960), intrinsics)
        self.assertTrue(result['passed'])
        self.assertFalse(result['lens_refitted'])
        self.assertLess(result['rms_pixels'], .2)
        incompatible = {**intrinsics, 'fx': intrinsics['fx'] * 1.8, 'fy': intrinsics['fy'] * .7}
        self.assertFalse(calibration.fixed_lens_validation(views, (1280, 960), incompatible)['passed'])

    def test_validate_command_keeps_profile_and_checks_variant(self):
        views, _, _ = self.synthetic_views()
        fit = calibration.fit_views(views, (1280, 960))
        args = argparse.Namespace(board='synthetic', sensor='fixture', lens='fixture', capture_mode='full-frame',
                                  hardware_id='source-unit', scope='model', origin='simulated', id='test-profile', name='Test profile')
        path = self.root / 'profile.json'
        calibration.write_json(path, calibration.calibration_profile(args, (1280, 960), fit, 'a'*64))
        original = path.read_bytes()
        args.profile, args.images, args.out = path, self.root, self.root / 'validation'
        args.columns, args.rows, args.square_mm = 9, 6, 20
        args.hardware_id = 'validation-unit'
        with patch.object(calibration, 'detect_views', return_value=(views, (1280, 960), [])):
            calibration.validate_command(args)
            self.assertEqual(path.read_bytes(), original)
            report = json.loads((args.out / 'validation.json').read_text())
            self.assertTrue(report['result']['passed'])
            self.assertEqual(report['camera']['hardware_id'], 'validation-unit')
            args.sensor = 'different-sensor'; args.out = self.root / 'mismatch'
            with self.assertRaisesRegex(ValueError, 'sensor does not match'):
                calibration.validate_command(args)
            self.assertFalse(args.out.exists())

    def test_target_is_vector_a4_and_never_overwrites(self):
        path = self.root / 'target.pdf'
        calibration.target_pdf(path)
        data = path.read_bytes()
        self.assertTrue(data.startswith(b'%PDF-1.4'))
        self.assertIn(b'/MediaBox [0 0 841.889764 595.275591]', data)
        self.assertIn(b'exactly 100 mm', data)
        self.assertEqual(data.count(b'56.692913 56.692913 re f'), 35)
        with self.assertRaises(FileExistsError):
            calibration.target_pdf(path)
        with self.assertRaises(ValueError):
            calibration.target_pdf(self.root / 'oversize.pdf', square_mm=50)

    def test_model_profile_keeps_origin_and_reference_device(self):
        views, _, _ = self.synthetic_views()
        result = calibration.fit_views(views, (1280, 960))
        args = argparse.Namespace(board='seeed-xiao-esp32s3-sense', sensor='ov3660', lens='stock',
                                  capture_mode='full-frame-unmirrored-unflipped', hardware_id='synthetic-camera',
                                  scope='model', origin='simulated', id='synthetic-profile', name='Synthetic calibration')
        profile = calibration.calibration_profile(args, (1280, 960), result, 'a'*64)
        self.assertEqual(profile['origin'], 'simulated')
        self.assertEqual(profile['scope'], 'model')
        self.assertNotIn('hardware_id', profile['camera'])
        self.assertEqual(profile['evidence']['reference_device_ids'], ['synthetic-camera'])
        self.assertEqual(len(profile['intrinsics']['distortion']), 5)
        args.scope = 'device'
        self.assertEqual(calibration.calibration_profile(args, (1280, 960), result, 'a'*64)['camera']['hardware_id'], 'synthetic-camera')

    def test_capture_manifest_rejects_relabelled_origin_and_changed_images(self):
        manifest = {'schema': 'wendy.camera-calibration.capture.v1', 'origin': 'simulated', 'hardware_id': 'test-camera',
                    'frames': [{'file': 'one.jpg', 'sha256': 'a'*64}]}
        calibration.write_json(self.root / 'capture.json', manifest)
        views = [{'file': 'one.jpg', 'sha256': 'a'*64}]
        with self.assertRaisesRegex(ValueError, 'origin or hardware'):
            calibration.validate_manifest(self.root, views, 'measured', 'test-camera')
        with self.assertRaisesRegex(ValueError, 'digest'):
            calibration.validate_manifest(self.root, [{'file': 'one.jpg', 'sha256': 'b'*64}], 'simulated', 'test-camera')
        self.assertEqual(calibration.validate_manifest(self.root, views, 'simulated', 'test-camera')['origin'], 'simulated')

    def test_capture_preserves_receipt_and_stops_on_session_change(self):
        cv2, np = calibration.libraries()
        ok, encoded = cv2.imencode('.jpg', np.zeros((64, 64), np.uint8))
        self.assertTrue(ok)
        data = encoded.tobytes(); digest = hashlib.sha256(data).hexdigest()
        receipt = {'sha256': digest, 'size': len(data), 'width': 64, 'height': 64,
                   'origin': 'simulated', 'source_session': 'first', 'content_type': 'image/jpeg', 'representation': 'original_jpeg'}
        requests = []
        def read(path, limit):
            requests.append(path)
            if path.startswith('/v1/evidence/'):
                return data
            r = {**receipt}
            if requests.count('/v1/twin') > 1:
                r['source_session'] = 'second'
                r['sha256'] = 'b'*64
            return json.dumps({'cameras': [{'id': 'camera-1', 'state': {'fresh': True, 'latest': r}}]}).encode()
        args = argparse.Namespace(viewer_profile=self.root / 'unused', hardware_id='operator-unit', out=self.root / 'capture',
                                  camera='camera-1', count=15, interval=.5, duration=30, columns=9, rows=6, square_mm=20)
        corners = np.zeros((54, 1, 2), np.float32)
        with patch.object(calibration, 'viewer_reader', return_value=read), patch.object(calibration.time, 'sleep'), \
             patch.object(calibration.time, 'monotonic', side_effect=itertools.count()), \
             patch.object(cv2, 'findChessboardCornersSB', return_value=(True, corners)):
            with self.assertRaisesRegex(ValueError, 'reconnected'):
                calibration.capture_command(args)
        saved = json.loads((args.out / 'capture.json').read_text())
        self.assertEqual(saved['origin'], 'simulated')
        self.assertEqual(saved['identity_source'], 'operator_declared')
        self.assertFalse(saved['complete'])
        self.assertEqual(saved['frames'][0]['receipt'], receipt)
        self.assertEqual((args.out / '000.jpg').read_bytes(), data)

    def test_image_header_limits_and_read_only_local_endpoint(self):
        with self.assertRaises(ValueError): calibration.image_dimensions(b'not an image')
        data = b'\x89PNG\r\n\x1a\n' + b'\x00'*8 + (100000).to_bytes(4, 'big')*2
        with self.assertRaisesRegex(ValueError, 'dimensions'):
            calibration.image_dimensions(data)
        for url in ('https://example.com', 'http://127.0.0.1@evil.test', 'http://localhost/path', 'http://127.0.0.1/?token=x'):
            p = self.root / 'profile.json'; p.write_text(json.dumps({'url': url, 'token': 'test'}))
            with self.assertRaises(ValueError): calibration.viewer_reader(p)


if __name__ == '__main__':
    unittest.main()
