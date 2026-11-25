"""Read-only native 3D mirror of an owned live-office simulation fixture.

Run with MuJoCo 4's mjpython on macOS. Only GET /v1/samples is used. This
viewer never steps physics or sends actuator commands; missions stay in the
operator app. The model is the exact digest-bound package used by the site.
"""
import argparse
import json
import math
from pathlib import Path
import subprocess
import time
import urllib.request

from sim.run import compile_model, load_package


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("simulation viewer refuses redirects")


def validate_snapshot(snapshot, ready, package, previous=None):
    state = snapshot["state"]
    if (snapshot.get("schema_version") != "wendy.sim.live.v1"
            or snapshot.get("run_id") != ready["run_id"]
            or state.get("synthetic") is not True
            or state.get("site_id") != package["site_id"]
            or state.get("frame") != {"id": "office-floor", "revision": package["frame_revision"]}
            or state.get("provenance", {}).get("mode") != "simulated"
            or state["provenance"].get("package_sha256") != package["_digest"]
            or snapshot.get("boot_id") != state.get("boot_id")
            or not isinstance(state.get("boot_id"), str) or not state["boot_id"]):
        raise ValueError("capture does not belong to the selected synthetic scene")
    position, velocity = state["position_m"], state["velocity_mps"]
    for values in (position, velocity):
        if (not isinstance(values, list) or len(values) != 3
                or any(type(v) not in (int, float) or not math.isfinite(v) for v in values)):
            raise ValueError("invalid scene position or velocity")
    seconds, sequence = state["simulation_time_s"], snapshot["sequence"]
    if (type(seconds) not in (int, float) or not math.isfinite(seconds) or seconds < 0
            or type(sequence) is not int or sequence < 1
            or abs(position[1] - .2) > .001 or abs(position[2]) > .001
            or not -2.001 <= position[0] <= 2.001
            or any(abs(v) > .001 for v in velocity[1:])):
        raise ValueError("unsupported one-axis fixture state")
    if previous is not None and (state["boot_id"] != previous["boot_id"]
            or seconds < previous["seconds"] or sequence < previous["sequence"]
            or sequence == previous["sequence"] and seconds != previous["seconds"]):
        raise ValueError("scene restarted or capture clock regressed")
    return {"boot_id": state["boot_id"], "seconds": seconds, "sequence": sequence,
            "position": position, "velocity": velocity}


def connection(path, package):
    ready = json.loads(path.read_text())
    port = ready.get("scene_port")
    if (ready.get("mission_schema") != "wendy.native-mission.site.v1"
            or ready.get("package_sha256") != package["_digest"]
            or type(port) is not int or not 1 <= port <= 65535):
        raise ValueError("select a matching native simulation fixture ready.json")
    inspected = subprocess.run(["docker", "inspect", ready["scene_name"]],
                               check=True, capture_output=True, text=True, timeout=10)
    scene = json.loads(inspected.stdout)[0]
    if (scene["Config"]["Labels"].get("wendy.world.native-test") != ready["ownership_label"]
            or scene["Image"] != ready["scene_image"] or not scene["State"]["Running"]
            or scene["NetworkSettings"]["Ports"].get("8080/tcp") !=
            [{"HostIp": "127.0.0.1", "HostPort": str(port)}]):
        raise ValueError("scene endpoint no longer belongs to this fixture")
    token = (path.parent / "sensor.token").read_text().strip()
    request = urllib.request.Request(f"http://127.0.0.1:{port}/v1/samples",
                                     headers={"Authorization": "Bearer " + token})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def read():
        with opener.open(request, timeout=.75) as response:
            raw = response.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError("capture exceeds viewer bound")
        return json.loads(raw)
    return ready, read


def run(args):
    def progress(stage):
        print(json.dumps({"viewer": "starting", "stage": stage}), flush=True)

    progress("import MuJoCo")
    import mujoco
    import mujoco.viewer
    if mujoco.__version__ != "4.0.0":
        raise ValueError("use the pinned MuJoCo 4.0.0 environment")
    progress("validate package and fixture")
    package = load_package(args.package)
    ready, read = connection(args.ready, package)
    progress("compile model")
    model = compile_model(package, mujoco)
    if (model.nq, model.nv, model.nu) != (1, 1, 1):
        raise ValueError("viewer only supports the commissioned one-axis live-office fixture")
    body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, "inspection-puck")
    if body <= 0 or package["mujoco"]["body"] != "inspection-puck":
        raise ValueError("unsupported robot model")
    data = mujoco.MjData(model)
    progress("read initial capture")
    current = validate_snapshot(read(), ready, package)
    deadline = time.monotonic() + args.duration
    last_changed = time.monotonic()
    count = 0
    progress("open native window")
    with mujoco.viewer.launch_passive(model, data, show_left_ui=False, show_right_ui=False) as viewer:
        progress("first frame")
        with viewer.lock():
            viewer.cam.lookat[:] = [0, .5, .35]
            viewer.cam.distance = max(8, float(model.stat.extent) * 1.35)
            viewer.cam.azimuth = 120
            viewer.cam.elevation = -48
        while viewer.is_running() and time.monotonic() < deadline:
            started = time.monotonic()
            label = "LIVE CAPTURE"
            try:
                sample = validate_snapshot(read(), ready, package, current)
                if sample["sequence"] != current["sequence"]:
                    last_changed = time.monotonic()
                current = sample
                if time.monotonic() - last_changed > 1:
                    label = "STALE: last observed pose"
            except ValueError:
                raise  # Changed identity or malformed state must never be rendered.
            except (OSError, TimeoutError):
                label = "DISCONNECTED: last observed pose"
            with viewer.lock():
                data.qpos[0] = current["position"][0] - float(model.body_pos[body, 0])
                data.qvel[0] = current["velocity"][0]
                data.time = current["seconds"]
                data.ctrl[:] = 0
                data.xfrc_applied[:] = 0
                mujoco.mj_forward(model, data)
            viewer.set_texts((None, None,
                "WENDY WORLD | SYNTHETIC SIMULATION\nRead-only 3D view | " + label,
                f"Robot X {current['position'][0]:.2f} m | speed {current['velocity'][0]:.2f} m/s\n"
                f"Simulation {current['seconds']:.1f} s | capture {current['sequence']}\n"
                "Drag to orbit | scroll to zoom | authorize inspections in Wendy World"))
            viewer.sync()
            count += 1
            if count == 1:
                print(json.dumps({"viewer": "ready", "run_id": ready["run_id"],
                                  "package_sha256": package["_digest"], "mujoco": mujoco.__version__,
                                  "read_only": True}), flush=True)
            time.sleep(max(0, .2 - (time.monotonic() - started)))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ready", type=Path, required=True)
    parser.add_argument("--package", type=Path)
    parser.add_argument("--duration", type=int, default=1800)
    # Cocoa reads NSArgumentDomain before mjpython invokes this parser. Accept
    # only the launcher's explicit override; do not ignore arbitrary arguments.
    parser.add_argument("-ApplePersistenceIgnoreState", choices=("YES",), help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    if not 1 <= args.duration <= 3600:
        parser.error("duration must be 1..3600 seconds")
    if args.package is None:
        root = Path(__file__).resolve().parents[1]
        relative = json.loads(args.ready.read_text()).get("world_package", "examples/live-office/world.json")
        args.package = (root / relative).resolve()
        if not args.package.is_relative_to(root / "examples"):
            parser.error("fixture package must remain inside repository examples")
    run(args)


if __name__ == "__main__":
    main()
