"""Actual MuJoCo rendering and sensor-file contract checks."""
import hashlib
import io
import json
import math
from pathlib import Path
import struct
import tempfile
import unittest
import wave
import zlib

from sim.sensors import SENSORS, audio_wav, parameters, samples, sensor_package, write_run
from sim.run import PackageError

PACKAGE = Path(__file__).resolve().parents[1] / "examples/sensor-office/world.json"


def pixels(png):
    # The fixture encoder emits one RGB IDAT with PNG filter zero on each row.
    assert png[:8] == b"\x89PNG\r\n\x1a\n"
    cursor, compressed = 8, bytearray()
    while cursor < len(png):
        size = struct.unpack_from(">I", png, cursor)[0]
        kind = png[cursor+4:cursor+8]
        data = png[cursor+8:cursor+8+size]
        crc = struct.unpack_from(">I", png, cursor+8+size)[0]
        assert crc == zlib.crc32(kind+data) & 0xffffffff
        if kind == b"IDAT": compressed.extend(data)
        cursor += size + 12
    raw = zlib.decompress(compressed)
    assert len(raw) == 240 * (320*3+1)
    return [tuple(raw[row*961+1+col*3:row*961+4+col*3]) for row in range(240) for col in range(320)]


def red_fraction(png):
    rgb = pixels(png)
    return sum(r > 150 and r > 1.5*g and r > 1.5*b for r,g,b in rgb)/len(rgb)


class SensorFixtureTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.package = sensor_package(PACKAGE)
        cls.warning = list(samples(cls.package, frames=2, interval_ms=500, run_id="unit"))
        cls.clear = list(samples(cls.package, frames=1, run_id="clear", alarm=False))

    def test_rendered_cameras_are_distinct_and_detectable(self):
        cameras = [(s,b) for s,b in self.warning if s["kind"] == "camera"]
        self.assertEqual(len(cameras),4)
        self.assertNotEqual(cameras[0][1],cameras[1][1])
        # Advancing actual MuJoCo physics changes the blue puck in the view.
        self.assertNotEqual(cameras[0][1],cameras[2][1])
        for s,b in cameras:
            self.assertGreater(red_fraction(b),.01)
            self.assertEqual(hashlib.sha256(b).hexdigest(),s["evidence"]["sha256"])
            self.assertEqual(len(b),s["evidence"]["size"])
        for s,b in self.clear:
            if s["kind"] == "camera": self.assertLess(red_fraction(b),.001)

    def test_camera_geometry_is_explicit_proper_rotation(self):
        import numpy as np
        for s,_ in self.warning:
            if s["kind"] != "camera": continue
            c=s["camera"]; t=np.array(c["camera_to_site"]).reshape(4,4)
            self.assertTrue(np.isfinite(t).all())
            self.assertTrue(np.allclose(t[3],[0,0,0,1]))
            self.assertTrue(np.allclose(t[:3,3],s["position_m"]))
            self.assertTrue(np.allclose(t[:3,:3].T @ t[:3,:3],np.eye(3)))
            self.assertAlmostEqual(np.linalg.det(t[:3,:3]),1)
            # Fixed cameras target indicator center, which projects to center.
            target=np.array([0,.65,0,1])
            camera=np.linalg.inv(t) @ target
            self.assertGreater(camera[2],0)
            self.assertAlmostEqual(c["fx"]*camera[0]/camera[2]+c["cx"],160,places=6)
            self.assertAlmostEqual(c["fy"]*camera[1]/camera[2]+c["cy"],120,places=6)

    def test_gas_and_audio_are_separate_labeled_fixture_models(self):
        warning_gas=next(s for s,_ in self.warning if s["kind"]=="gas")
        clear_gas=next(s for s,_ in self.clear if s["kind"]=="gas")
        self.assertGreater(warning_gas["gas"]["value_ppm"],45)
        self.assertLess(clear_gas["gas"]["value_ppm"],2)
        self.assertEqual(warning_gas["gas"]["species"],"synthetic-test-gas")
        for s,b in self.warning+self.clear:
            if s["kind"]!="audio":continue
            with wave.open(io.BytesIO(b),"rb") as w:
                self.assertEqual((w.getnchannels(),w.getsampwidth(),w.getframerate(),w.getnframes()),(1,2,8000,2000))
                pcm=struct.unpack("<2000h",w.readframes(2000))
            rms=math.sqrt(sum((v/32768)**2 for v in pcm)/len(pcm))
            self.assertEqual(s["audio"]["duration_ns"],250_000_000)
            self.assertTrue(rms>.2 if s["run_id"]=="unit" else rms<.003)
            self.assertEqual(s["sensor_model"],"generated-880hz-tone-v1")

    def test_lineage_order_and_sensor_omission(self):
        for s,b in self.warning:
            self.assertEqual(s["schema_version"],"wendy.sensor.sample.v1")
            self.assertEqual(s["id"],f"unit/{s['sensor_id']}/{s['sequence']}")
            self.assertEqual(s["sample_ns"],250_000_000+(s["sequence"]-1)*500_000_000)
            self.assertEqual(s["package_sha256"],self.package["_digest"])
            self.assertEqual(s["scenario_version"],"sensor-office-fixture-v1")
            self.assertEqual(s["frame"],{"id":"office-floor","revision":"office-frame-v1"})
            self.assertGreater(s["uncertainty_m"],0)
            self.assertEqual(b is None,s["kind"]=="gas")
        partial=list(samples(self.package,frames=1,omit=["audio-1","camera-b"]))
        self.assertEqual([s["sensor_id"] for s,_ in partial],["camera-a","gas-1"])

    def test_repeatable_same_renderer_and_audio_seed(self):
        again=list(samples(self.package,frames=2,interval_ms=500,run_id="unit"))
        self.assertEqual(again,self.warning)
        self.assertNotEqual(audio_wav(7,1,True),audio_wav(8,1,True))

    def test_invalid_parameters_fail_before_output(self):
        base=dict(frames=1,interval_ms=100,seed=7,run_id="ok",alarm=True,omit=[])
        for field,value in [("frames",True),("frames",1001),("interval_ms",11),("seed",-1),("run_id","../bad"),("alarm",1),("omit",None),("omit",["unknown"]),("omit",list(SENSORS)),("omit",["gas-1","gas-1"])]:
            with self.subTest(field=field,value=value):
                args=base|{field:value}
                with self.assertRaises(PackageError):parameters(**args)
        with tempfile.TemporaryDirectory() as tmp:
            out=Path(tmp)/"run"
            with self.assertRaises(PackageError):write_run(PACKAGE,out,frames=0)
            self.assertFalse(out.exists())

    def test_private_output_digest_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            out=Path(tmp)/"run"
            report=write_run(PACKAGE,out,frames=1,run_id="files")
            self.assertTrue(report["synthetic"])
            self.assertEqual(report["samples"],4)
            self.assertEqual(report["configuration"]["seed"],7)
            rows=[json.loads(line) for line in (out/"samples.jsonl").read_text().splitlines()]
            self.assertEqual(len(rows),4)
            for row in rows:
                if "evidence" not in row:continue
                path=out/"blobs"/row["evidence"]["sha256"]
                self.assertEqual(path.stat().st_mode & 0o777,0o600)
                self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(),path.name)
            with self.assertRaises(FileExistsError):write_run(PACKAGE,out,frames=1)


if __name__ == "__main__": unittest.main()
