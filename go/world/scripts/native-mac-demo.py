#!/usr/bin/env python3
"""Build and run the private native Mac simulation demo, using existing fixtures."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from integration import nativeapp


def runtime_inputs():
    paths = [ROOT.parents[1] / name for name in ("go.mod", "go.sum")]
    paths += [ROOT / name for name in ("tests/native_mission_site.py",
        "tests/native_client_site.py", "tests/virtual_site.py", "sim/requirements.txt")]
    for directory, pattern in (("internal", "*.go"), ("cmd/world-lab", "*.go"), ("sim", "*.py")):
        paths.extend((ROOT / directory).rglob(pattern) if directory != "sim" else (ROOT / directory).glob(pattern))
    return {os.path.relpath(path, ROOT): hashlib.sha256(path.read_bytes()).hexdigest() for path in sorted(paths)}


def require_version(text, pattern, minimum, label):
    match = re.search(pattern, text)
    if not match or tuple(map(int, match.groups())) < minimum:
        raise RuntimeError(f"{label} {'.'.join(map(str, minimum))} or newer is required; found {text.splitlines()[0]}")


def captured(argv, timeout=30):
    result = subprocess.run(argv, cwd=ROOT, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{argv[0]} failed: {result.stderr.strip()[-1000:]}")
    return result.stdout.strip()


def preflight(app=None):
    app = nativeapp.executable(app)
    if platform.system() != "Darwin":
        raise RuntimeError("The native Mac demo requires macOS 14 or newer.")
    if int(platform.mac_ver()[0].split(".")[0]) < 14:
        raise RuntimeError("The native Mac app requires macOS 14 or newer.")
    for executable in ("docker", "go", "python3.12"):
        if not shutil.which(executable):
            raise RuntimeError(f"Install {executable} before starting this demo.")
    context = captured(["docker", "context", "show"])
    endpoint = json.loads(captured(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if os.environ.get("DOCKER_HOST") and not os.environ.get("DOCKER_CONTEXT"):
        endpoint = os.environ["DOCKER_HOST"]
    if not endpoint.startswith("unix://"):
        raise RuntimeError("Use a local Docker Unix-socket context; remote engines are outside this demo.")
    server = json.loads(captured(["docker", "info", "--format", "{{json .}}"], 45))
    if server.get("OSType") != "linux":
        raise RuntimeError("A running local Docker Linux engine is required.")
    go = captured(["go", "version"])
    require_version(go, r"go(\d+)\.(\d+)", (1, 27), "Go")
    print("Preflight passed: local Docker Linux engine, Go and integrated WendyDesktop are available.", flush=True)
    return {"docker_context": context, "docker_server": server.get("ServerVersion"), "go": go, "app": app}



def new_directory(path):
    path = path.resolve()
    # Demo keys and generated artifacts must remain in the repository's ignored tree.
    if not path.is_relative_to(ROOT / ".local") or path == ROOT / ".local":
        raise ValueError("Choose a new directory inside this repository's .local directory.")
    path.mkdir(mode=0o700, parents=True, exist_ok=False)
    return path


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")
    path.chmod(0o600)


def logged(argv, logfile, timeout=1800):
    print(f"Running {argv[0]} {argv[1]}; log: {logfile.relative_to(ROOT)}", flush=True)
    started = time.monotonic()
    with logfile.open("w") as output:
        result = subprocess.run(argv, cwd=ROOT, stdout=output, stderr=subprocess.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"Command failed with exit {result.returncode}; inspect {logfile}")
    return round(time.monotonic() - started, 3)


def build(folder, environment):
    inputs = runtime_inputs()
    folder.mkdir(mode=0o700, parents=True, exist_ok=False)
    logs = folder / "logs"
    logs.mkdir(mode=0o700)
    suffix = uuid.uuid4().hex[:12]
    node = f"wendy-world-node:mac-demo-{suffix}"
    scene = f"wendy-world-live:mac-demo-{suffix}"
    timings = {}
    for name, dockerfile, image in (("node", "deploy/node.Dockerfile", node), ("scene", "sim/demo.Dockerfile", scene)):
        context = ROOT.parents[1] if name == "node" else ROOT
        timings[name] = logged(["docker", "build", "--pull=false", "-f", str(ROOT / dockerfile), "-t", image, str(context)], logs / f"{name}-build.log")
    app = Path(environment['app'])
    app_sha256 = hashlib.sha256(app.read_bytes()).hexdigest()
    viewer = folder / "viewer-venv"
    logged(["python3.12", "-m", "venv", str(viewer)], logs / "viewer-venv.log")
    timings["viewer"] = logged([str(viewer / "bin/python"), "-m", "pip", "install", "-r",
        str(ROOT / "sim/requirements.txt")], logs / "viewer-install.log")
    libdir = captured([str(viewer / "bin/python"), "-c", "import sysconfig; print(sysconfig.get_config_var('LIBDIR'))"])
    package_path = "examples/demo-office/world.json"
    if runtime_inputs() != inputs:
        raise RuntimeError("Runtime or commissioning source changed during the build; repeat from a new output directory.")
    result = {"schema": "wendy.native-mac-demo.build.v1", "app": str(app), "app_sha256": app_sha256, "environment": environment,
        "node_image": captured(["docker", "image", "inspect", node, "--format", "{{.Id}}"]),
        "scene_image": captured(["docker", "image", "inspect", scene, "--format", "{{.Id}}"]),
        "local_tags": [node, scene], "build_seconds": timings,
        "viewer_python": str(viewer / "bin/mjpython"), "viewer_libdir": libdir,
        "world_package": package_path, "runtime_inputs": inputs,
        "package_sha256": hashlib.sha256((ROOT / package_path).read_bytes()).hexdigest()}
    write_json(folder / "build.json", result)
    return result


def load_build(folder):
    folder = folder.resolve()
    if not folder.is_relative_to(ROOT / ".local"):
        raise ValueError("Build artifacts must be inside this repository's .local directory.")
    result = json.loads((folder / "build.json").read_text())
    if result.get("schema") != "wendy.native-mac-demo.build.v1":
        raise ValueError("Unsupported build receipt.")
    app = Path(result["app"]).resolve()
    if nativeapp.executable(app) != str(app) or hashlib.sha256(app.read_bytes()).hexdigest() != result.get("app_sha256"):
        raise ValueError("The integrated native app changed; prepare a new build receipt.")
    viewer = Path(result["viewer_python"]).resolve()
    if not viewer.is_relative_to(folder) or not viewer.is_file():
        raise ValueError("The 3D viewer is absent or outside its build directory.")
    if (result.get("world_package") != "examples/demo-office/world.json" or
            hashlib.sha256((ROOT / result["world_package"]).read_bytes()).hexdigest() != result.get("package_sha256")):
        raise ValueError("The demo package changed; build a new demo before starting.")
    for key in ("node_image", "scene_image"):
        actual = captured(["docker", "image", "inspect", result[key], "--format", "{{.Id}}"])
        if actual != result[key]:
            raise ValueError("The pinned local image is unavailable.")
    return result


def start(folder, artifacts, duration):
    if runtime_inputs() != artifacts.get("runtime_inputs"):
        raise ValueError("Runtime or commissioning source changed; rebuild before starting this demo.")
    site = folder / "site"
    ready = site / "ready.json"
    started = time.monotonic()
    process = None
    viewer = None
    viewer_log = None
    try:
        with (folder / "site.log").open("w") as output:
            process = subprocess.Popen([sys.executable, str(ROOT / "tests/native_mission_site.py"),
                "--out", str(site), "--duration", str(duration), "--node-image", artifacts["node_image"],
                "--scene-image", artifacts["scene_image"], "--package", artifacts["world_package"]], cwd=ROOT, stdout=output,
                stderr=subprocess.STDOUT, start_new_session=True)
            print(f"Starting the virtual site; log: {folder.relative_to(ROOT)}/site.log", flush=True)
            deadline = time.monotonic() + 240
            info = None
            while info is None:
                if process.poll() is not None:
                    raise RuntimeError(f"Virtual site exited during startup; inspect {folder / 'site.log'}")
                if time.monotonic() >= deadline:
                    raise RuntimeError("Virtual site did not become ready within four minutes.")
                if ready.exists():
                    try:
                        info = json.loads(ready.read_text())
                    except json.JSONDecodeError:
                        pass  # The fixture writes this small file before announcing readiness.
                if info is not None:
                    break
                time.sleep(.2)
            if info.get("mission_schema") != "wendy.native-mission.site.v1":
                raise ValueError("Fixture returned an unexpected readiness record.")
            profile = Path(info["native_mission_profiles"]["macos"]).resolve()
            if not profile.is_relative_to(site) or not profile.is_file():
                raise ValueError("Fixture returned an unexpected profile path.")
            write_json(folder / "demo.json", {"schema": "wendy.native-mac-demo.run.v1", "run_id": info["run_id"],
                "ready": str(ready), "app": artifacts["app"], "startup_seconds": round(time.monotonic()-started, 3),
                "node_image": artifacts["node_image"], "scene_image": artifacts["scene_image"]})
            # Leave this explicitly launched app independent of the fixture.
            # No application-name termination or existing-app routing.
            subprocess.Popen([artifacts["app"], "--world-profile", str(profile)], start_new_session=True,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            environment = dict(os.environ)
            # uv-managed Python can otherwise fail to resolve its libpython under mjpython.
            environment["DYLD_LIBRARY_PATH"] = artifacts["viewer_libdir"]
            viewer_log = (folder / "viewer.log").open("w")
            viewer_log.write(json.dumps({"viewer": "starting", "stage": "macOS bootstrap"}) + "\n")
            viewer_log.flush()
            # AppKit consumes this process-local argument before mjpython starts
            # our Python code. A prior GLFW crash must not block on restoration.
            viewer = subprocess.Popen([artifacts["viewer_python"], "-m", "sim.live_viewer", "--ready", str(ready),
                "--package", str(ROOT / artifacts["world_package"]), "--duration", str(duration),
                "-ApplePersistenceIgnoreState", "YES"],
                cwd=ROOT, env=environment, stdout=viewer_log, stderr=subprocess.STDOUT)
            viewer_deadline = time.monotonic() + 30
            while True:
                if viewer.poll() is not None:
                    raise RuntimeError(f"3D viewer exited during startup with code {viewer.returncode}; inspect {folder / 'viewer.log'}")
                lines = (folder / "viewer.log").read_text()[-16384:].splitlines()
                if any('"viewer": "ready"' in line for line in lines):
                    break
                if time.monotonic() >= viewer_deadline:
                    raise RuntimeError(f"3D viewer did not become ready; inspect {folder / 'viewer.log'}")
                time.sleep(.2)
            print("Wendy World and the 3D office viewer are opening. Review inspections in the Mac app's Missions tab.", flush=True)
            print(f"This is synthetic simulation. No mission has been sent. The site runs for up to {duration} seconds.", flush=True)
            print(f"Stop with Ctrl-C here, or create {site / 'stop'}. The app can remain open with its last-known snapshot.", flush=True)
            while process.poll() is None:
                time.sleep(.5)
            if process.returncode:
                raise RuntimeError(f"Virtual site exited with {process.returncode}; inspect {folder / 'site.log'}")
    finally:
        try:
            if viewer is not None and viewer.poll() is None:
                viewer.terminate()
                try:
                    viewer.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    viewer.kill()
                    viewer.wait(timeout=5)
        finally:
            if viewer_log is not None:
                viewer_log.close()
            if process is not None and process.poll() is None:
                # SIGTERM asks the fixture's handler to run ownership-scoped cleanup.
                process.terminate()
                try:
                    process.wait(timeout=120)
                except subprocess.TimeoutExpired as error:
                    raise RuntimeError(f"Fixture cleanup is still running, PID {process.pid}; inspect {folder / 'site.log'}") from error
        if process is not None:
            print(f"Fixture process ended. Private logs and profiles remain in {folder}.", flush=True)


def interrupt(signum, frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", type=Path, help="integrated Wendy.app or WendyDesktop executable")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("preflight", help="check local prerequisites without building or starting the site")
    compile_cmd = sub.add_parser("build", help="build isolated local images using the installed native Mac app")
    compile_cmd.add_argument("--out", type=Path, required=True, help="new directory under .local")
    run = sub.add_parser("start", help="build, start a bounded site and open the native Mac app")
    run.add_argument("--out", type=Path, required=True, help="new directory under .local")
    run.add_argument("--build", type=Path, help="reuse a previous launcher's build directory")
    run.add_argument("--duration", type=int, default=1800, help="site lifetime in seconds, 30..3600")
    args = parser.parse_args()
    if args.command == "start" and not 30 <= args.duration <= 3600:
        parser.error("--duration must be between 30 and 3600 seconds")
    os.umask(0o077)
    signal.signal(signal.SIGTERM, interrupt)
    environment = preflight(args.app)
    if args.command == "preflight":
        return
    folder = new_directory(args.out)
    if args.command == "build":
        build(folder / "build", environment)
        print(f"Build ready: {folder / 'build'}", flush=True)
        return
    artifacts = load_build(args.build) if args.build else build(folder / "build", environment)
    start(folder, artifacts, args.duration)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("Demo stopped.", file=sys.stderr)
        sys.exit(130)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f"Demo failed: {error}", file=sys.stderr)
        sys.exit(1)
