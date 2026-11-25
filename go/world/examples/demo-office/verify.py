"""Check package, original actuator parity and actual rendered fusion inputs."""
import argparse
import hashlib
import json
from pathlib import Path
import struct
import sys
import zlib

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))


def png(rgb):
    def chunk(kind, value):
        return struct.pack(">I", len(value)) + kind + value + struct.pack(">I", zlib.crc32(kind + value) & 0xffffffff)
    height, width, channels = rgb.shape
    if channels != 3 or str(rgb.dtype) != "uint8":
        raise ValueError("Expected RGB renderer bytes")
    pixels = b"".join(b"\x00" + row.tobytes() for row in rgb)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(pixels, 6)) + chunk(b"IEND", b""))


def verify(out):
    import mujoco
    import numpy as np
    from sim.live import live_package
    from sim.run import compile_model

    if mujoco.__version__ != "4.0.0":
        raise ValueError("Use the repository's pinned MuJoCo 4.0.0 environment")
    out = out.resolve()
    if not out.is_relative_to(ROOT / ".local"):
        raise ValueError("Keep verification artifacts inside the private .local directory")
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    package = live_package(ROOT / "examples/demo-office/world.json")
    original = compile_model(live_package(ROOT / "examples/live-office/world.json"), mujoco)
    model = compile_model(package, mujoco)
    if model.nq != 1 or model.nv != 1 or model.nu != 1:
        raise AssertionError("One-axis adapter contract changed")
    body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, "inspection-puck")
    camera_b = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_CAMERA, "camera-b")
    if model.cam_bodyid[camera_b] != body:
        raise AssertionError("Camera-b must remain mounted on the test robot")
    np.testing.assert_array_equal(model.cam_pos[camera_b], [0, -2, 1.8])
    np.testing.assert_array_equal(model.body_inertia[body], [.016, .016, .016])
    if model.body_mass[body] != 1:
        raise AssertionError("Original mass changed")
    old, data = mujoco.MjData(original), mujoco.MjData(model)
    max_difference = 0.0
    for i in range(1200):
        for m, d in ((original, old), (model, data)):
            d.ctrl[0] = .1 if i < 600 else -.1
            mujoco.mj_step(m, d)
        max_difference = max(max_difference, float(np.max(np.abs(old.qpos-data.qpos))), float(np.max(np.abs(old.qvel-data.qvel))))
    if max_difference != 0:
        raise AssertionError(f"Actuator trajectory changed: {max_difference}")
    report = {"schema": "wendy.office-demo.scene-check.v1", "mujoco": mujoco.__version__,
        "package_sha256": package["_digest"], "ngeom": model.ngeom, "nq": model.nq, "nu": model.nu,
        "parity_steps": 1200, "max_qpos_qvel_difference": max_difference,
        "camera_b_local_offset_m": [0, 1.8, 2], "camera_red_fractions": {},
        "limits": ["synthetic geometry and analytic sensing", "one-axis ideal-brake test robot",
                   "host compilation/rendering/stepping only; no live distributed deployment or hardware"]}
    with mujoco.Renderer(model, height=240, width=320) as renderer:
        for q in (0, 1, 2, 3, 4):
            data.qpos[0] = q
            mujoco.mj_forward(model, data)
            # Match internal/livecapture/client.go's explicit local-frame offset.
            native_offset = data.cam_xpos[camera_b] - data.xpos[body]
            local_offset = native_offset[[0, 2, 1]] * [1, 1, -1]
            np.testing.assert_allclose(local_offset, [0, 1.8, 2], rtol=0, atol=1e-12)
            for camera in ("camera-a", "camera-b"):
                renderer.update_scene(data, camera=camera)
                rgb = renderer.render()
                # Exact predicate from internal/fusion/media.go, over opaque RGB.
                fraction = float(((rgb[:,:,0] > 120) & (rgb[:,:,0] > 1.8*rgb[:,:,1]) & (rgb[:,:,0] > 1.8*rgb[:,:,2])).mean())
                if fraction < .005:
                    raise AssertionError(f"{camera} at q={q} does not meet the unchanged fixture rule: {fraction}")
                name = f"{camera}-q{q}"
                report["camera_red_fractions"][name] = fraction
                (out / (name + ".png")).write_bytes(png(rgb))
    data.qpos[0] = 0
    mujoco.mj_forward(model, data)
    # High-resolution review output is separate from the live 320x240 framebuffer.
    model.vis.global_.offwidth = 1440
    model.vis.global_.offheight = 900
    with mujoco.Renderer(model, height=900, width=1440) as renderer:
        renderer.update_scene(data, camera="overview")
        (out / "overview.png").write_bytes(png(renderer.render()))
    report["files_sha256"] = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(out.glob("*.png"))}
    (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True, help="fresh private directory under .local")
    args = parser.parse_args()
    print(json.dumps(verify(args.out), indent=2))
