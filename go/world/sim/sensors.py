"""Render a bounded synthetic camera/gas/audio fixture into digest-addressed files.

Images come from MuJoCo's RGB renderer. Gas is an analytic field and audio is a
generated test tone, neither a validated physical hazard model. Output is the
versioned sensor adapter contract, not authenticated World publication.
"""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import math
import os
from pathlib import Path
import random
import re
import struct
import wave
import zlib

from sim.run import PackageError, compile_model, gas_value, load_package

SENSORS = ("camera-a", "camera-b", "gas-1", "audio-1")
WIDTH, HEIGHT = 320, 240


def png_rgb(rgb):
    """Encode actual HxWx3 renderer bytes as an RGB PNG without extra packages."""
    if rgb.shape != (HEIGHT, WIDTH, 3) or str(rgb.dtype) != "uint8":
        raise PackageError("renderer returned an unexpected image shape or type")
    def chunk(kind, value):
        return struct.pack(">I", len(value)) + kind + value + struct.pack(">I", zlib.crc32(kind + value) & 0xffffffff)
    pixels = b"".join(b"\x00" + row.tobytes() for row in rgb)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", WIDTH, HEIGHT, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(pixels, 6)) + chunk(b"IEND", b"")


def audio_wav(seed, sequence, alarm):
    """Generate a labeled PCM fixture, not a MuJoCo acoustic simulation."""
    rng = random.Random((seed << 32) | sequence)
    pcm = bytearray()
    for i in range(2000):
        noise = rng.uniform(-0.002, 0.002)
        sample = (0.4 * math.sin(2 * math.pi * 880 * i / 8000) if alarm else 0) + noise
        pcm.extend(struct.pack("<h", round(sample * 32767)))
    out = io.BytesIO()
    with wave.open(out, "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(8000)
        wav.writeframes(pcm)
    return out.getvalue()


def sensor_package(path):
    package = load_package(path)
    cfg = package.get("sensors")
    expected = {"cameras": ["camera-a", "camera-b"], "width": WIDTH, "height": HEIGHT,
                "indicator_material": "indicator-material", "gas_position_m": [0, .2, 0],
                "audio_position_m": [0, .5, 0], "audio_rate_hz": 8000, "audio_duration_ms": 250,
                "alarm_frequency_hz": 880, "calibration_revision": "synthetic-sensors-v1"}
    if cfg != expected:
        raise PackageError("unsupported sensor package bindings")
    return package


def parameters(frames, interval_ms, seed, run_id, alarm, omit):
    if type(frames) is not int or not 1 <= frames <= 1000:
        raise PackageError("frames must be an integer from 1 to 1000")
    if type(interval_ms) is not int or not 10 <= interval_ms <= 1000 or interval_ms % 10:
        raise PackageError("interval-ms must be 10..1000 in multiples of 10")
    if type(seed) is not int or not 0 <= seed < 2**32:
        raise PackageError("seed must be an unsigned 32-bit integer")
    if not isinstance(run_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", run_id):
        raise PackageError("run ID must contain 1..80 letters, digits, underscores or hyphens")
    if type(alarm) is not bool or not isinstance(omit, (list, tuple)) or any(name not in SENSORS for name in omit) or len(set(omit)) != len(omit) or len(omit) == len(SENSORS):
        raise PackageError("invalid alarm or omitted sensor configuration")


def calibration(model, data, camera_id):
    # MuJoCo cameras use +X right,+Y up,-Z forward. Convert into computer-vision
    # +X right,+Y down,+Z forward, then MuJoCo Z-up into site X,Z,-Y.
    import numpy as np
    to_site = np.array([[1., 0., 0.], [0., 0., 1.], [0., -1., 0.]])
    rotation = to_site @ data.cam_xmat[camera_id].reshape(3, 3) @ np.diag([1., -1., -1.])
    position = to_site @ data.cam_xpos[camera_id]
    transform = np.eye(4)
    transform[:3, :3] = rotation
    transform[:3, 3] = position
    fovy = float(model.cam_fovy[camera_id])
    if not 0 < fovy < 180 or not np.isfinite(transform).all():
        raise PackageError("invalid camera calibration")
    focal = HEIGHT / (2 * math.tan(math.radians(fovy) / 2))
    return position.tolist(), {"width": WIDTH, "height": HEIGHT, "fx": focal, "fy": focal,
                             "cx": WIDTH / 2, "cy": HEIGHT / 2,
                             "camera_to_site": transform.reshape(-1).tolist(),
                             "calibration_valid_for_ns": 60_000_000_000}


def samples(package, *, frames=3, interval_ms=100, seed=7, run_id="sensor-smoke", alarm=True, omit=()):
    """Yield (sample, optional media bytes) in simulation-time order."""
    parameters(frames, interval_ms, seed, run_id, alarm, omit)
    import mujoco
    model = compile_model(package, mujoco)
    data = mujoco.MjData(model)
    if abs(model.opt.timestep - .01) > 1e-12:
        raise PackageError("sensor fixture requires the declared 10ms physics step")
    material = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_MATERIAL, "indicator-material")
    actuator = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_ACTUATOR, package["mujoco"]["actuator"])
    camera_ids = {name: mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_CAMERA, name) for name in SENSORS[:2]}
    if material < 0 or actuator < 0 or any(i < 0 for i in camera_ids.values()):
        raise PackageError("missing sensor fixture model binding")
    model.mat_rgba[material] = [1., .02, .02, 1.] if alarm else [.02, .8, .02, 1.]
    rng = random.Random(seed)
    with mujoco.Renderer(model, height=HEIGHT, width=WIDTH) as renderer:
        for sequence in range(1, frames + 1):
            sample_ns = 250_000_000 + (sequence - 1) * interval_ms * 1_000_000
            while round(float(data.time) * 1_000_000_000) < sample_ns:
                data.ctrl[actuator] = package["mujoco"]["control"]
                mujoco.mj_step(model, data)
            mujoco.mj_forward(model, data)
            for sensor_id in SENSORS:
                if sensor_id in omit:
                    continue
                kind = "camera" if sensor_id.startswith("camera") else "gas" if sensor_id == "gas-1" else "audio"
                sample = {"schema_version": "wendy.sensor.sample.v1", "id": f"{run_id}/{sensor_id}/{sequence}",
                          "site_id": package["site_id"], "frame": {"id": "office-floor", "revision": package["frame_revision"]},
                          "sensor_id": sensor_id, "kind": kind, "sequence": sequence, "sample_ns": sample_ns,
                          "run_id": run_id, "scenario_version": package["package_revision"], "seed": seed,
                          "package_sha256": package["_digest"], "uncertainty_m": .01,
                          "calibration_revision": package["sensors"]["calibration_revision"]}
                media = None
                if kind == "camera":
                    sample["sensor_model"] = f"mujoco-{mujoco.__version__}-rgb-pinhole"
                    sample["position_m"], sample["camera"] = calibration(model, data, camera_ids[sensor_id])
                    renderer.update_scene(data, camera=sensor_id)
                    media = png_rgb(renderer.render())
                    content_type = "image/png"
                elif kind == "gas":
                    sample["sensor_model"] = "analytic-gaussian-fixture-v1"
                    sample["position_m"] = package["sensors"]["gas_position_m"]
                    gas = dict(package["gas_model"])
                    if not alarm:
                        gas["peak_ppm"] = 0
                    sample["gas"] = {"value_ppm": gas_value(gas, sample["position_m"], rng),
                                     "uncertainty_ppm": gas["noise_stddev_ppm"], "species": "synthetic-test-gas"}
                else:
                    sample["sensor_model"] = "generated-880hz-tone-v1"
                    sample["position_m"] = package["sensors"]["audio_position_m"]
                    sample["audio"] = {"sample_rate_hz": 8000, "channels": 1, "duration_ns": 250_000_000}
                    media, content_type = audio_wav(seed, sequence, alarm), "audio/wav"
                if media is not None:
                    sample["evidence"] = {"sha256": hashlib.sha256(media).hexdigest(), "content_type": content_type, "size": len(media)}
                yield sample, media


def write_run(path, out, *, frames=3, interval_ms=100, seed=7, run_id="sensor-smoke", alarm=True, omit=()):
    # Validate before creating any files or importing the renderer.
    parameters(frames, interval_ms, seed, run_id, alarm, omit)
    kwargs = dict(frames=frames, interval_ms=interval_ms, seed=seed, run_id=run_id, alarm=alarm, omit=list(omit))
    package = sensor_package(path)
    out = Path(out)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    blobs = out / "blobs"
    blobs.mkdir(mode=0o700)
    count, total_bytes = 0, 0
    with (out / "samples.jsonl").open("x", encoding="utf-8") as stream:
        os.chmod(out / "samples.jsonl", 0o600)
        for sample, media in samples(package, **kwargs):
            if media is not None:
                blob = blobs / sample["evidence"]["sha256"]
                if not blob.exists():
                    with blob.open("xb") as target:
                        os.chmod(blob, 0o600)
                        target.write(media)
                    total_bytes += len(media)
            stream.write(json.dumps(sample, sort_keys=True, allow_nan=False) + "\n")
            count += 1
    report = {"schema_version": "wendy.sensor.run.v1", "synthetic": True, "package_sha256": package["_digest"],
              "configuration": kwargs, "samples": count, "blob_bytes": total_bytes,
              "fidelity": package["fidelity"], "asset_license": package["asset_license"]}
    with (out / "run.json").open("x", encoding="utf-8") as stream:
        os.chmod(out / "run.json", 0o600)
        stream.write(json.dumps(report, indent=2, sort_keys=True) + "\n")
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--out", type=Path, required=True, help="new private output directory")
    parser.add_argument("--frames", type=int, default=3)
    parser.add_argument("--interval-ms", type=int, default=100)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--run-id", default="sensor-smoke")
    parser.add_argument("--alarm", choices=("true", "false"), default="true")
    parser.add_argument("--omit", action="append", choices=SENSORS, default=[])
    args = parser.parse_args(argv)
    try:
        report = write_run(args.package, args.out, frames=args.frames, interval_ms=args.interval_ms, seed=args.seed, run_id=args.run_id, alarm=args.alarm == "true", omit=args.omit)
        print(json.dumps(report, sort_keys=True))
        return 0
    except (OSError, ValueError, ImportError) as error:
        print(f"sensor simulation failed: {error}", file=__import__("sys").stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
