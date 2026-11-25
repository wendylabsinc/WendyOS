"""Run a local firmware-camera cue and a real articulated Go2 inspection.

The optical sequence is rendered ahead of time from the same MuJoCo geometry.
Only JPEG bytes returned by the emulated firmware can trigger perception. The
existing simulation-only mission executor owns all robot commands.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import urllib.request
import uuid

from integration import nativeapp
from integration.xiaofire.perception import Detector, validate_calibration
from integration.xiaofire.scene import projected_visible
from sim.fire_navigation import prepare_response
from sim.patrol_viewer import atomic, display

ROOT = Path(__file__).resolve().parents[2]


def request(base, path, body=None, token=None, timeout=5):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), headers=headers)
    # These explicit local runtime endpoints must not inherit a machine's
    # outbound proxy configuration or send private evidence to that proxy.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=timeout) as response:
        return json.load(response)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def command(argv, **kw):
    return subprocess.check_output(argv, cwd=ROOT, text=True, timeout=kw.pop("timeout", 30), **kw).strip()


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def load_stimulus_calibration(scene, stimuli):
    """Check the original artifact bytes before adapting its run identity."""
    raw = (scene / "calibration.json").read_bytes()
    if hashlib.sha256(raw).hexdigest() != stimuli.get("calibration_sha256"):
        raise ValueError("rendered optical calibration changed")
    return json.loads(raw)


def wait(getter, predicate, seconds=90):
    deadline, last = time.monotonic() + seconds, None
    while time.monotonic() < deadline:
        try:
            value = getter()
            if predicate(value):
                return value
        except Exception as error:
            last = str(error)
        time.sleep(.2)
    raise RuntimeError("runtime readiness deadline: " + str(last))


class FrameTail:
    def __init__(self, directory):
        self.directory, self.offsets = directory, {}

    def read(self):
        rows = []
        for path in sorted(self.directory.glob("capture-*/frames.jsonl")):
            with path.open("rb") as stream:
                stream.seek(self.offsets.get(path, 0))
                while True:
                    start = stream.tell()
                    line = stream.readline()
                    if not line.endswith(b"\n"):
                        stream.seek(start)
                        break
                    row = json.loads(line)
                    image = (path.parent / row["jpeg_path"]).resolve()
                    if not image.is_relative_to(path.parent.resolve()):
                        raise ValueError("camera evidence path escaped its capture")
                    stamp = row.get("received_unix_ns")
                    if type(stamp) is not int:
                        raise ValueError("observer must retain its host receipt timestamp")
                    age = (time.time_ns() - stamp) / 1e9
                    # This is an explicit same-host receipt clock adapter, not
                    # a conversion of the firmware's boot clock to UTC.
                    if age < 0 or age >= 2:
                        continue
                    rows.append((row, image, time.monotonic() - age))
                self.offsets[path] = stream.tell()
        return rows


def passive_state(physical, sequence):
    robot = dict(id="go2", name="Go2", color=[.12, .5, .7], position=physical["position"],
                 velocity=physical["linear_velocity_world"], online=True, available=True, state="waiting for camera",
                 job_id="", path=[], lidar_points=physical.get("lidar_points", []), local_voxels=0, peer_voxels=0, route=[])
    snapshot = dict(run_id=physical["run_id"], time_s=physical["stamp"], phase="waiting_for_fire",
                    authorized=False, stopped=True, fault=physical.get("fault"), robots=[robot], jobs=[], completed=0, total=1,
                    events=[], voxels=[], voxel_size=.1, map_node="go2", network=dict(edges=[], internet=dict(id="internet",
                    a="go2", b="cloud", enabled=False, up=False, packets=0), gossip_rounds=0, peer_voxels={}), controls_available=False)
    return dict(schema="wendy.macos-demo.state.v1", mode="go2-fire-response", run_id=physical["run_id"],
                sequence=sequence, worker_status="running", last_command_sequence=0, snapshot=snapshot,
                dynamic_geoms=physical["dynamic_geoms"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("out", "scene", "firmware-bundle", "emulator", "observer", "go2-source", "assets"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--physics-image", required=True)
    parser.add_argument("--navigation-image", required=True)
    parser.add_argument("--app", type=Path)
    parser.add_argument("--seconds", type=int, default=600)
    args = parser.parse_args()
    out, scene = args.out.resolve(), args.scene.resolve()
    if out.is_relative_to(ROOT) or out.exists() or not 60 <= args.seconds <= 1200:
        parser.error("use a fresh external output directory and a 60..1200 second limit")
    os.umask(0o077)
    out.mkdir(parents=True, mode=0o700)
    bundle = args.firmware_bundle.resolve()
    firmware = json.loads((bundle / "build-receipt.json").read_text())
    if firmware["source_differences"] or sha(bundle / "blank-flash.bin") != firmware["artifacts"]["flash"]["sha256"]:
        raise ValueError("verified unchanged firmware source and pinned UART image required")
    docker = json.loads(command(["docker", "info", "--format", "{{json .}} "]))
    context = command(["docker", "context", "show"])
    endpoint = json.loads(command(["docker", "context", "inspect", context]))[0]["Endpoints"]["docker"]["Host"]
    if os.environ.get("DOCKER_HOST") and not os.environ.get("DOCKER_CONTEXT"):
        endpoint = os.environ["DOCKER_HOST"]
    if docker.get("OSType") != "linux" or not endpoint.startswith("unix://"):
        raise ValueError("this fixture requires a local Linux Docker engine")
    run_id = "xiao-fire-" + uuid.uuid4().hex[:12]
    stimuli = json.loads((scene / "stimuli.json").read_text())
    calibration = load_stimulus_calibration(scene, stimuli)
    calibration["run_id"] = run_id
    calibration_hash = validate_calibration(calibration, run_id, (2048, 1536))
    atomic(out / "calibration.json", calibration)
    if sha(scene/"office.xml") != stimuli["scene"]["office_sha256"] or any(sha(scene/row["path"]) != row["sha256"] for row in stimuli["frames"]):
        raise ValueError("rendered optical input changed")
    def outside_camera(route):
        for a, b in zip(route, route[1:]):
            steps = max(1, math.ceil(math.dist(a[:2], b[:2])/.15))
            for i in range(steps+1):
                t = i/steps
                if projected_visible(calibration, [a[0]+t*(b[0]-a[0]), .35, -a[1]-t*(b[1]-a[1])], radius=.65):
                    return False
        return True
    images = {name: command(["docker", "image", "inspect", tag, "--format", "{{.Id}}"])
              for name, tag in (("physics", args.physics_image), ("navigation", args.navigation_image))}
    report = dict(schema="wendy.xiao-go2-fire.run.v1", run_id=run_id, simulation=True, hardware=False,
                  firmware_flash_sha256=firmware["artifacts"]["flash"]["sha256"], images=images,
                  scope="Pre-rendered synthetic optical input through actual ESP32 firmware; pixel inference; signed Go2 inspection",
                  passed=False, events=[], checks={}, scene_model_sha256=calibration["model_sha256"],
                  emulator_sha256=sha(args.emulator), observer_sha256=sha(args.observer),
                  stimuli_manifest_sha256=sha(scene/"stimuli.json"), calibration_sha256=calibration_hash,
                  source_sha256={name:sha(ROOT/name) for name in (
                      "integration/xiaofire/run.py", "integration/xiaofire/perception.py", "integration/xiaofire/scene.py",
                      "sim/fire_navigation.py", "sim/patrol_physics.py", "sim/patrol_nav.py", "sim/patrol_localization.py",
                      "internal/patrol/patrol.go", "internal/patrol/return.go", "internal/patrol/localization.go")})
    processes, containers = [], []
    network = run_id + "-network"
    network_created = False
    mission_url, mission_token = None, None

    def spawn(argv, name):
        with (out / (name + ".log")).open("wb") as log:
            process = subprocess.Popen(argv, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        processes.append((name, process))
        return process

    def event(kind, **values):
        report["events"].append(dict(event=kind, unix_ms=int(time.time()*1000), **values))
        atomic(out / "run.json", report)

    try:
        with (out / "build.log").open("wb") as log:
            for name in ("world-patrol", "world-camera"):
                subprocess.run(["go", "build", "-o", str(out/name), "./cmd/"+name], cwd=ROOT,
                               stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
        command(["docker", "network", "create", network]); network_created = True
        physics_name, nav_name = run_id + "-physics", run_id + "-nav"
        command(["docker", "run", "-d", "--name", physics_name, "--network", network, "--network-alias", "physics",
                 "--restart", "no", "--cpus", "4", "--memory", "3g", "--user", "0:0", "-p", "127.0.0.1::8080",
                 "-v", str(ROOT)+":/world:ro", "-v", str(args.go2_source.resolve())+":/source:ro",
                 "-v", str(args.assets.resolve())+":/assets:ro", "-v", str(scene)+":/fixture:ro",
                 "--pull=never", images["physics"], "--source", "/source", "--assets", "/assets",
                 "--office", "/fixture/office.xml", "--run-id", run_id]); containers.append(physics_name)
        physics = "http://" + command(["docker", "port", physics_name, "8080/tcp"])
        wait(lambda: request(physics, "/sensors"), lambda x: x.get("ready"))
        scope = request(physics, "/state")
        if scope["model_sha256"] != calibration["model_sha256"]:
            raise ValueError("camera rendering and robot physics have different geometry")
        command(["docker", "run", "-d", "--name", nav_name, "--network", network, "--restart", "no",
                 "--cpus", "3", "--memory", "2g", "-p", "127.0.0.1::8092", "--pull=never", images["navigation"],
                 "--physics", "http://physics:8080", "--frame-id", scope["frame"]["id"], "--frame-revision", scope["frame"]["revision"]]); containers.append(nav_name)
        navigation = "http://" + command(["docker", "port", nav_name, "8092/tcp"])
        nav = wait(lambda: request(navigation, "/status"), lambda x: x.get("ready"), 120)
        candidates = request(navigation, "/candidates")
        nav_frame = request(navigation, "/policy/context")["frame"]
        session = out / "viewer"; session.mkdir()
        for name in ("evidence", "commands", "receipts"):
            (session/name).mkdir()
        geometry = request(physics, "/geometry", timeout=20)
        geometry["presentation"].update(name="ESP32 camera / Go2 response", subtitle="Actual firmware and articulated Go2 simulation")
        atomic(session / "geometry.json", geometry)
        atomic(session / "bridge-session.json", dict(schema="wendy.macos-demo.session.v1", run_id=run_id, pid=os.getpid(), restart="refused"))
        atomic(session / "state.json", passive_state(scope, 1))
        if args.app:
            spawn([nativeapp.executable(args.app), "--world-session", str(session)], "viewer")
        bridge = out / "firmware-bridge"
        spawn([sys.executable, str(ROOT/"integration/xiaofirmware/serial_bridge.py"), "--emulator", str(args.emulator.resolve()),
               "--firmware", str(bundle/"blank-flash.bin"), "--elf", str(bundle/"build/wendy_mcu.elf"), "--frames", str(scene/"frames"),
               "--uart-port", str(free_port()), "--seconds", str(args.seconds), "--out", str(bridge)], "firmware")
        selector = wait(lambda: json.loads((bridge/"selector.json").read_text()), lambda x: x.get("status")=="running", 15)
        camera_dir = out / "camera"; camera_dir.mkdir()
        source = out / "camera-source"
        source.write_text("#!"+sys.executable+"\nimport os,uuid\n"+
                          "binary="+repr(str(args.observer.resolve()))+"\n"+
                          "out="+repr(str(camera_dir))+"+'/capture-'+uuid.uuid4().hex\n"+
                          "os.execv(binary,[binary,'--serial-pty',"+repr(selector["serial_port"])+",'--out',out,'--duration','120s','--stdout-jpeg'])\n")
        source.chmod(0o700)
        camera_state = camera_dir / "state"; camera_state.mkdir(mode=0o700)
        spawn([str(out/"world-camera"), "--device", "wendy-lite:simulated-xiao", "--camera-id", "0",
               "--state-dir", str(camera_state), "--simulation-run", run_id, "--emulated-source", str(source),
               "--duration", str(args.seconds)+"s"], "camera")
        camera_profile = wait(lambda: json.loads((camera_state/"viewer-profile.json").read_text()), lambda x: bool(x.get("url")), 15)
        report.update(physics_url=physics, navigation_url=navigation, session=str(session), initial_position=scope["position"])
        event("camera_started_robot_waiting")
        detector, tail = Detector(run_id), FrameTail(camera_dir)
        started, deadline = time.monotonic(), time.monotonic()+args.seconds-10
        latest, latest_image, latest_row, prepared, accepted, mission = None, None, None, None, None, None
        sequence, command_sequence, observed_clean, observed_frames = 1, 0, 0, 0
        trace = (out/"timeline.jsonl").open("w")
        physics_trace = (out/"physics.jsonl").open("w")
        detection_trace = (out/"detections.jsonl").open("w")
        next_state = 0.
        pre_commands, positions, joints, contacts, pre_positions = [], [], [], [], []
        fov_clear = True
        attempted_frame = None
        clean_before_dispatch = 0
        camera_live_at_inspection = None
        while time.monotonic() < deadline:
            now = time.monotonic()
            for row, path, received in tail.read():
                raw = path.read_bytes()
                latest = detector.detect(raw, row, calibration, received, time.monotonic())
                latest_image, latest_row = path, row
                observed_frames += 1
                observed_clean += int(not latest["candidate"])
                clean_before_dispatch += int(not latest["candidate"] and accepted is None)
                detection_trace.write(json.dumps(latest)+"\n"); detection_trace.flush()
                evidence = session/"evidence"/(row["sha256"]+".jpg")
                if not evidence.exists(): evidence.write_bytes(raw)
            frame_key = (latest_row["session_id"], latest_row["frame_seq"]) if latest_row else None
            if latest and latest["detected"] and accepted is None and frame_key != attempted_frame and time.monotonic()<latest["expires_monotonic_s"]:
                received_state = request(camera_profile["url"], "/v1/state", token=camera_profile["token"])
                matching = [x for x in received_state["recent"] if x["sha256"]==latest_row["sha256"] and x["simulation_run"]==run_id]
                if matching:
                    attempted_frame = frame_key
                    try:
                        prepared = prepare_response(latest, latest_image, scope, request(navigation,"/status"), candidates,
                                                    lambda body: request(navigation,"/route",body,timeout=12),
                                                    navigation_frame=nav_frame, calibration_sha256=calibration_hash, route_acceptor=outside_camera)
                        if time.monotonic() >= prepared["expires_monotonic_s"]:
                            raise ValueError("camera evidence expired during route preparation")
                    except ValueError as error:
                        event("response_not_prepared", reason=str(error), attempts=getattr(error,"attempts",[]))
                        prepared = None
                    if prepared:
                        accepted = latest
                        atomic(out/"world-camera-receipt.json",matching[-1])
                        atomic(out/"response.json", prepared)
                        atomic(out/"schedule.json", prepared["schedule"])
                        listen = "127.0.0.1:"+str(free_port()); mission_url="http://"+listen
                        spawn([str(out/"world-patrol"), "--out", str(out/"executor"), "--physics", physics,
                               "--navigation", navigation, "--listen", listen, "--duration", str(args.seconds),
                               "--schedule", str(out/"schedule.json")], "executor")
                        wait(lambda: request(mission_url,"/status"),lambda x:"patrol" in x,30)
                        mission_token=(out/"executor/control-token").read_text().strip()
                        event("confirmed_camera_cue_dispatched", image_sha256=latest_row["sha256"], position_m=latest["position_m"])
            if now >= next_state:
                physical=request(physics,"/state")
                if physical["run_id"] != run_id or physical["model_sha256"] != calibration["model_sha256"]:
                    raise ValueError("physics restarted or changed its model")
                positions.append(physical["position"])
                joints.append(physical["joints"]["q"])
                contacts.append(physical["foot_contacts"])
                if accepted is None:
                    pre_commands.append((physical["command"],physical["applied_command"]))
                    pre_positions.append(physical["position"])
                pos = physical["position"]
                if projected_visible(calibration, [pos[0],pos[2],-pos[1]], radius=.65):
                    fov_clear = False
                    event("camera_view_violation", position=pos, simulation_time_s=physical["stamp"], robot_bound_radius_m=.65)
                    raise RuntimeError("actual Go2 entered pre-rendered camera view; optical replay is no longer valid")
                sequence+=1
                if mission_url:
                    mission=request(mission_url,"/status")
                    if mission["run_id"] != run_id:
                        raise ValueError("mission run changed")
                    if camera_live_at_inspection is None and mission["patrol"].get("tour",{}).get("visits"):
                        camera_live_at_inspection = bool(latest and time.monotonic()<latest["source"]["expires_monotonic_s"])
                    state=display(physical,mission,sequence,command_sequence,session,out/"executor/evidence")
                    state["mode"]="go2-fire-response"
                else:
                    state=passive_state(physical,sequence)
                if latest:
                    fresh=time.monotonic()<latest.get("expires_monotonic_s",latest["source"]["expires_monotonic_s"])
                    candidate=latest["candidate"] and fresh
                    position=latest.get("position_m")
                    optical=calibration["camera_to_world"]
                    camera_position=[optical[3],-optical[11],optical[7]]
                    state["snapshot"]["fusion"]=dict(state="hazard" if candidate else "clear", label="Synthetic fire cue" if candidate else "Camera watching",
                        reason="Original JPEG through ESP32-S3 firmware and SensorLink. Fixture-specific pixel rule; simulated optics.",
                        sample_time_s=physical["stamp"],position=[position[0],-position[2],position[1]] if candidate else None,
                        confidence=None,uncertainty_m=.5 if candidate else None,basis="synthetic-flame-cue-v1",
                        observations=[dict(id="xiao-s3",kind="camera",label="ESP32-S3 / original firmware JPEG",position=camera_position,
                        target=[position[0],-position[2],position[1]] if candidate else None,active=fresh,
                        evidence_path="evidence/"+latest_row["sha256"]+".jpg")])
                path=session/"commands"/f"{command_sequence+1:06d}.json"
                if path.exists() and mission_url:
                    if path.is_symlink() or path.stat().st_size>4096:
                        raise ValueError("invalid native command file")
                    control=json.loads(path.read_text()); action=control.get("action","")
                    if (set(control)!={"schema","run_id","sequence","action","target"} or
                        control["schema"]!="wendy.macos-demo.command.v1" or control["run_id"]!=run_id or
                        control["sequence"]!=command_sequence+1 or control["target"]!="" or
                        action not in ("patrol_pause","patrol_resume","patrol_stop","patrol_recall")):
                        raise ValueError("invalid native control")
                    request(mission_url,"/control",{"action":action.removeprefix("patrol_")},mission_token,timeout=15)
                    command_sequence+=1
                    atomic(session/"receipts"/f"{command_sequence:06d}.json",dict(schema="wendy.macos-demo.command-result.v1",run_id=run_id,sequence=command_sequence,action=action,accepted=True,error=None))
                atomic(session/"state.json",state)
                at=time.monotonic()-started
                trace.write(json.dumps(dict(at_s=at,state=state))+"\n");trace.flush()
                physics_trace.write(json.dumps(dict(at_s=at,position=physical["position"],velocity=physical["linear_velocity_world"],joints=physical["joints"]["q"],foot_contacts=physical["foot_contacts"],command=physical["command"],applied_command=physical["applied_command"],nav=(mission or {}).get("navigation"),phase=state["snapshot"]["phase"]))+"\n");physics_trace.flush()
                next_state=now+.2
                if mission and mission["patrol"]["state"] in ("completed","failed","stopped","expired"):
                    report["terminal"]=mission
                    report["final_position"]=physical["position"]
                    break
            time.sleep(.03)
        for stream in (trace,physics_trace,detection_trace):stream.close()
        traveled = sum(math.dist(a[:2],b[:2]) for a,b in zip(positions,positions[1:]))
        excursions = [max(values)-min(values) for values in zip(*joints)] if joints else []
        report["measurements"] = dict(travelled_m=traveled, joint_excursions_rad=excursions,
            initial_incident_distance_m=prepared["initial_incident_distance_m"] if prepared else None,
            prepared_route_length_m=prepared["route_length_m"] if prepared else None,
            inspection_visits=(mission or {}).get("patrol",{}).get("tour",{}).get("visits",[]),
            pre_cue_samples=len(pre_commands), pre_cue_displacement_m=max((math.dist(p[:2],pre_positions[0][:2]) for p in pre_positions),default=0),
            contact_patterns=len({tuple(c) for c in contacts}),
            minimum_incident_distance_m=min((math.dist(p[:2],[accepted["position_m"][0],-accepted["position_m"][2]]) for p in positions),default=None) if accepted else None)
        report["checks"].update(no_walking_command_before_camera=bool(pre_commands) and all(abs(v)<1e-6 for pair in pre_commands for values in pair for v in values),
                                clean_frames_before_dispatch=clean_before_dispatch>0, camera_view_remained_clear=fov_clear,
                                actual_standoff=report["measurements"]["minimum_incident_distance_m"] is not None and report["measurements"]["minimum_incident_distance_m"]>=1.5,
                                actual_travel=traveled>=6, articulated_gait=len(excursions)==12 and min(excursions)>.05 and len({tuple(c) for c in contacts})>1,
                                original_frames_received=observed_frames>3, confirmed_pixel_cue=accepted is not None,
                                signed_mission_started=mission_url is not None,
                                camera_live_at_inspection=camera_live_at_inspection is True,
                                completed=bool(mission and mission["patrol"]["state"]=="completed"))
        report["observed_frames"]=observed_frames
        report["observed_clean_frames"]=observed_clean
        report["clean_frames_before_dispatch"]=clean_before_dispatch
        report["response_verified"] = bool(report["measurements"]["inspection_visits"]) and all(value for key,value in report["checks"].items() if key!="completed")
        report["return_complete"] = report["checks"]["completed"]
        report["passed"]=all(report["checks"].values())
    except (Exception, KeyboardInterrupt) as error:
        report["error"]=str(error) or "operator interrupted local run"
    finally:
        if mission_url and mission_token:
            try:request(mission_url,"/control",{"action":"stop"},mission_token)
            except Exception:pass
        for name,process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:process.wait(timeout=10)
                except subprocess.TimeoutExpired:process.kill();process.wait(timeout=5)
        for name in reversed(containers):
            logs=subprocess.run(["docker","logs","--tail","100",name],capture_output=True,timeout=15)
            (out/(name+".log")).write_bytes(logs.stdout+logs.stderr)
            subprocess.run(["docker","rm","-f",name],capture_output=True,check=True,timeout=20)
        if network_created:subprocess.run(["docker","network","rm",network],capture_output=True,check=True,timeout=20)
        report["cleanup"]="owned processes and containers stopped"
        atomic(out/"run.json",report)
    print(json.dumps({"passed":report["passed"],"run":str(out/"run.json"),"error":report.get("error")}))
    return 0 if report["passed"] else 1


if __name__=="__main__":
    raise SystemExit(main())
