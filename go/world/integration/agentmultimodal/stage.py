#!/usr/bin/env python3
"""Stage a private ordinary-app multimodal run; never launch its runtime."""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import posixpath
import re
import shutil
import stat
import subprocess
import tarfile
import uuid

ROOT = Path(__file__).resolve().parents[2]
STAGING = ROOT / ".local" / "agentmultimodal"
APP = "com.wendy.world.multimodal"
LABEL = "wendy.world.agentmultimodal.export"
MAX_ROOTFS = 2 << 30
CAPTURE = "wendy.sensor.capture.v1"
RESULT = "wendy.inference.result.v1"
SERVICES = ("scene", "camera-a", "gas", "audio", "sensorlink-source", "sensorlink-bridge", "fusion")
SENSORS = {"camera-a": "camera-a", "gas": "gas-1", "audio": "audio-1", "sensorlink-bridge": "camera-b"}
BINARIES = ("agent", "cli", "initializer", "fixture", "sensor", "sensorlink", "fusion", "app-runner")
SOURCE_FILES = ("sim/__init__.py", "sim/run.py", "sim/actuator.py", "sim/sensors.py", "sim/live.py", "sim/requirements.txt", "examples/live-office/world.json", "examples/live-office/office.xml")
IMAGE_ENV = {"PATH", "LANG", "GPG_KEY", "PYTHON_VERSION", "PYTHON_SHA256", "MUJOCO_GL", "MESA_SHADER_CACHE_DISABLE"}


class Refused(ValueError):
    pass


def strict_json(raw,limit=1<<20):
    if not isinstance(raw,bytes) or len(raw)>limit:
        raise Refused("bounded JSON bytes required")
    def pairs(items):
        result={}
        for key,value in items:
            if key in result:raise Refused("duplicate JSON field")
            result[key]=value
        return result
    def floating(value):
        result=float(value)
        if not math.isfinite(result):raise Refused("nonfinite JSON number")
        return result
    def constant(value):raise Refused("nonfinite JSON constant")
    return json.loads(raw,object_pairs_hook=pairs,parse_float=floating,parse_constant=constant)


def digest_file(path):
    h = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def json_bytes(value):
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def write_new(path, data, mode=0o600):
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, "wb") as out:
        out.write(data)
        out.flush()
        os.fsync(out.fileno())


def safe_output(path, staging=STAGING):
    path, staging = Path(path), Path(staging)
    if not path.is_absolute() or path != Path(os.path.abspath(path)) or path == staging or not path.is_relative_to(staging):
        raise Refused("new output must be below the private .local/agentmultimodal directory")
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise Refused("output must not cross symlinks")
        if parent.exists() and parent.stat().st_mode & 0o022:
            if parent == Path("/tmp") or parent == Path("/private/tmp"):
                continue
            raise Refused("output ancestors must not be writable by other users")
    if path.exists():
        raise Refused("output already exists; reuse is not allowed")
    path.mkdir(mode=0o700, parents=True)
    os.chmod(path, 0o700)
    return path


def regular(path, limit):
    path = Path(path)
    if not path.is_absolute() or path.is_symlink():
        raise Refused("explicit regular input required")
    st = path.stat()
    if not stat.S_ISREG(st.st_mode) or st.st_mode & 0o022 or not 0 < st.st_size <= limit:
        raise Refused("input type, permissions or size refused")
    return path


def immutable_image(value):
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", value):
        raise Refused("full immutable local image ID required")
    return value


def docker_json(args, run=subprocess.run):
    result = run(["docker", *args], capture_output=True, timeout=30, check=True)
    if len(result.stdout) > 1 << 20:
        raise Refused("Docker metadata exceeded its bound")
    return json.loads(result.stdout)


def image_info(image, scene=False, run=subprocess.run):
    immutable_image(image)
    rows = docker_json(["image", "inspect", image], run)
    if len(rows) != 1 or rows[0].get("Id") != image or rows[0].get("Os") != "linux" or rows[0].get("Architecture") != "arm64":
        raise Refused("exact Linux arm64 local image required")
    item = rows[0]
    result = {"id": image, "os": "linux", "architecture": "arm64", "size": item.get("Size")}
    if scene:
        cfg = item.get("Config", {})
        env = {}
        for entry in cfg.get("Env", []):
            key, sep, value = entry.partition("=")
            if not sep or key not in IMAGE_ENV or key in env:
                raise Refused("scene image has unexpected environment metadata")
            env[key] = value
        if cfg.get("User") != "65534:65534" or cfg.get("WorkingDir") != "/world" or cfg.get("Entrypoint") != ["python", "-m", "sim.live"] or env.get("MUJOCO_GL") != "osmesa" or env.get("MESA_SHADER_CACHE_DISABLE") != "true" or not env.get("PATH"):
            raise Refused("scene image configuration differs from the reviewed MuJoCo base")
        if cfg.get("Volumes"):
            raise Refused("export source must not declare anonymous volumes")
        result["environment"] = env
        result["user"] = cfg["User"]
        result["working_directory"] = cfg["WorkingDir"]
        result["entrypoint"] = cfg["Entrypoint"]
    return result


def tar_path(name):
    if not name or name.startswith("/") or "\\" in name or "\x00" in name:
        raise Refused("rootfs archive path refused")
    parts = PurePosixPath(name).parts
    if ".." in parts or len(name) > 4096:
        raise Refused("rootfs archive path escapes its root")
    return str(PurePosixPath(name))


def inspect_rootfs(path):
    regular(path, MAX_ROOTFS)
    allowed = {"bin", "boot", "dev", "etc", "home", "lib", "lib64", "media", "mnt", "opt", "proc", "root", "run", "sbin", "srv", "sys", "tmp", "usr", "var", "world", ".dockerenv"}
    seen, total, count, versions = set(), 0, 0, []
    with tarfile.open(path, "r:") as archive:
        for member in archive:
            name = tar_path(member.name)
            count += 1
            if name in seen or count > 100000:
                raise Refused("duplicate rootfs entry or archive entry budget exceeded")
            seen.add(name)
            if name == ".":
                if not member.isdir(): raise Refused("invalid archive root")
                continue
            top = name.split("/", 1)[0]
            if top not in allowed or name in {"token", "sensor-token", "metadata-read-token"} or name.startswith(("root/.ssh", "root/.aws", "root/.config", "var/lib/wendy", "etc/wendy-agent")):
                raise Refused("unreviewed or credential-bearing rootfs path")
            if not (member.isfile() or member.isdir() or member.issym() or member.islnk()):
                raise Refused("rootfs special devices and FIFOs are refused")
            if member.size < 0 or member.size > 512 << 20:
                raise Refused("rootfs member size refused")
            total += member.size
            if total > MAX_ROOTFS:
                raise Refused("rootfs expanded size exceeded")
            if member.issym() or member.islnk():
                link = member.linkname
                if "\x00" in link or "\\" in link:
                    raise Refused("rootfs link refused")
                target = posixpath.normpath(link.lstrip("/") if link.startswith("/") or member.islnk() else posixpath.join(posixpath.dirname(name), link))
                if target == ".." or target.startswith("../"):
                    raise Refused("rootfs link escapes image root")
            if name.endswith(".dist-info/METADATA") and "/mujoco_core-" in name:
                if member.size > 1 << 20:
                    raise Refused("MuJoCo metadata too large")
                body = archive.extractfile(member).read().decode("utf-8")
                if "\nName: mujoco-core\n" not in "\n"+body or "\nVersion: 4.0.0\n" not in "\n"+body:
                    raise Refused("MuJoCo 4.0.0 package required")
                versions.append(name)
    if len(versions) != 1 or "usr/local/bin/python" not in seen:
        raise Refused("rootfs lacks exact MuJoCo 4.0.0 or Python runtime")
    return {"sha256": digest_file(path), "size": path.stat().st_size, "expanded_size": total, "entries": count, "mujoco_distribution": versions[0], "mujoco_version": "4.0.0"}


def export_rootfs(image, output, run=subprocess.run, staging=STAGING):
    info = image_info(image, True, run)
    out = safe_output(output, staging)
    name = "world-mm-export-" + uuid.uuid4().hex[:16]
    token = uuid.uuid4().hex
    created = False
    try:
        created = True
        run(["docker", "create", "--name", name, "--label", LABEL+"="+token, "--network", "none", "--read-only", "--entrypoint", "/bin/false", image], capture_output=True, check=True, timeout=30)
        item = docker_json(["inspect", name], run)[0]
        if item.get("Config", {}).get("Labels", {}).get(LABEL) != token or item.get("Image") != image or item.get("State", {}).get("Status") != "created" or item.get("State", {}).get("Running") or item.get("State", {}).get("Pid") != 0 or item.get("Mounts"):
            raise Refused("export must use only this stopped, unmounted owned container")
        target = out/"rootfs.tar"
        # Docker writes to this fresh owner-only directory. No container process
        # starts; validation reads archive metadata without extracting files.
        run(["docker", "export", "--output", str(target), name], capture_output=True, check=True, timeout=120)
        os.chmod(target, 0o600)
        inventory = inspect_rootfs(target)
        manifest = {"version": 1, "kind": "wendy.agentmultimodal.base.v1", "image": info, "rootfs": inventory, "container_started": False}
        write_new(out/"base.json", json_bytes(manifest))
        return manifest
    finally:
        if created:
            item = docker_json(["inspect", name], run)[0]
            if item.get("Config", {}).get("Labels", {}).get(LABEL) != token or item.get("Image") != image or item.get("State", {}).get("Running"):
                raise Refused("refusing cleanup of changed export container")
            run(["docker", "rm", name], capture_output=True, check=True, timeout=30)


def volume_name(run_id, service, kind):
    return "world-mm-"+run_id+"-"+service+"-"+kind


def project_manifest(run_id):
    services = {}
    for service in SERVICES:
        ents = [{"type": "persist", "name": volume_name(run_id, service, "config"), "path": "/config"}, {"type": "persist", "name": volume_name(run_id, service, "proofs"), "path": "/proofs"}]
        if service in SENSORS:
            ents.append({"type": "world", "publish": [{"componentType": CAPTURE, "stream": SENSORS[service]}]})
        elif service == "fusion":
            ents.append({"type": "world", "publish": [{"componentType": RESULT, "stream": "hazard"}], "subscribe": [{"app": APP, "service": svc, "componentType": CAPTURE, "stream": sensor} for svc,sensor in SENSORS.items()]})
        services[service] = {"context": "services/"+service, "entitlements": ents}
    return {"appId": APP, "version": "1.0.0", "platform": "linux", "isolation": "isolated", "services": services}


def validate_project(project, run_id):
    if project != project_manifest(run_id):
        raise Refused("project must have exact isolated services, grants and distinct volumes")


def scene_command(run_id, anchor, seed):
    return ["/usr/local/bin/python", "-m", "sim.live", "examples/live-office/world.json", "--token-file", "/config/actuator-token", "--sensor-token-file", "/config/scene-read-token", "--run-id", run_id, "--seed", str(seed), "--interval-ms", "200", "--host", "0.0.0.0", "--port", "8091", "--allow-network-simulation", "--clock-domain", "simulation:"+run_id, "--clock-anchor-unix-ns", str(anchor)]


def app_command(service):
    if service in ("camera-a", "gas", "audio"):
        return ["/app", "--config", "/config/config.json"]
    if service in ("sensorlink-source", "sensorlink-bridge"):
        return ["/app", "--mode", "source" if service.endswith("source") else "bridge", "--config", "/config/config.json"]
    return ["/app", "--config", "/config/config.json"]


def dockerfile(service, run_id, anchor, seed, env):
    command = scene_command(run_id, anchor, seed) if service == "scene" else app_command(service)
    lines = ["FROM scratch"]
    if service == "scene":
        lines += ["ADD rootfs.tar /", "COPY sim /world/sim", "COPY examples /world/examples", "WORKDIR /world"]
        for key in ("PATH", "LANG", "MUJOCO_GL", "MESA_SHADER_CACHE_DISABLE"):
            lines.append("ENV "+key+"="+json.dumps(env[key]))
    else:
        lines.append("COPY app /app")
    lines += ["COPY app-runner /app-runner", "USER "+("65534:65534" if service in ("scene", "sensorlink-source") else "0:0")]
    duration = "600s" if service == "scene" else "300s"
    gate=[] if service=="scene" else ["--start-file","/config/start"]
    lines.append("ENTRYPOINT "+json.dumps(["/app-runner", "--directory", "/proofs", "--duration", duration, "--stdout-limit", str(32 << 20), "--stderr-limit", str(64 << 10), *gate, "--", *command]))
    return ("\n".join(lines)+"\n").encode()


def elf_arm64(path):
    path = regular(path, 512 << 20)
    with path.open("rb") as source: header = source.read(64)
    if len(header) < 64 or header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != 183:
        raise Refused("Linux arm64 ELF binary required")
    return {"sha256": digest_file(path), "size": path.stat().st_size}


def prepare(base, output, runtime_image, binaries, run_id, anchor, seed, run=subprocess.run, staging=STAGING):
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,39}", run_id) or type(anchor) is not int or not 0 < anchor < (1 << 63)-int(24*3600*1e9) or type(seed) is not int or not 0 <= seed < 1 << 32:
        raise Refused("bounded run, fixed anchor and seed required")
    if set(binaries) != set(BINARIES):
        raise Refused("exact root-built tool and app binaries required")
    base = Path(base)
    manifest = json.loads(regular(base/"base.json", 64 << 10).read_bytes())
    if manifest.get("kind") != "wendy.agentmultimodal.base.v1" or manifest.get("version") != 1 or manifest.get("container_started") is not False:
        raise Refused("validated stopped-container base export required")
    if image_info(manifest["image"]["id"], True, run) != manifest["image"]:
        raise Refused("base image metadata changed or manifest was edited")
    rootfs = inspect_rootfs(base/"rootfs.tar")
    if rootfs != manifest.get("rootfs"):
        raise Refused("base export changed after validation")
    runtime_info = image_info(runtime_image, False, run)
    bin_inventory = {name: elf_arm64(path) for name,path in binaries.items()}
    source_inventory = {name: {"sha256": digest_file(regular(ROOT/name, 8 << 20)), "size": (ROOT/name).stat().st_size} for name in SOURCE_FILES}
    world = json.loads((ROOT/"examples/live-office/world.json").read_bytes())
    if world["mujoco"]["path"] != "office.xml" or world["mujoco"]["sha256"] != source_inventory["examples/live-office/office.xml"]["sha256"]:
        raise Refused("scene package does not match its declared model")
    out = safe_output(output, staging)
    project = out/"project"
    spec = project_manifest(run_id)
    write_new(project/"wendy.json", json_bytes(spec), 0o644)
    for service in SERVICES:
        context = project/"services"/service
        write_new(context/"Dockerfile", dockerfile(service, run_id, anchor, seed, manifest["image"]["environment"]), 0o644)
        shutil.copyfile(binaries["app-runner"], context/"app-runner"); os.chmod(context/"app-runner", 0o755)
        if service == "scene":
            shutil.copyfile(base/"rootfs.tar", context/"rootfs.tar"); os.chmod(context/"rootfs.tar",0o600)
            for name in SOURCE_FILES:
                write_new(context/name, (ROOT/name).read_bytes(), 0o644)
        else:
            name = "sensor" if service in ("camera-a", "gas", "audio") else "sensorlink" if service.startswith("sensorlink-") else "fusion"
            shutil.copyfile(binaries[name], context/"app"); os.chmod(context/"app",0o755)
    (out/"tools").mkdir(mode=0o700)
    for name in ("agent", "cli", "initializer", "fixture"):
        shutil.copyfile(binaries[name],out/"tools"/name); os.chmod(out/"tools"/name,0o755)
    # Build contexts contain no secrets. COPY must preserve readable source
    # directories for the scene's UID65534; the enclosing stage remains0700.
    for directory in project.rglob("*"):
        if directory.is_dir():os.chmod(directory,0o755)
    plan = {"version":1,"kind":"wendy.agentmultimodal.plan.v1","simulation_only":True,"app":APP,"run_id":run_id,"seed":seed,"anchor_unix_ns":anchor,"clock_domain":"simulation:"+run_id,"site":world["site_id"],"frame":{"id":"office-floor","revision":world["frame_revision"]},"scenario_version":world["package_revision"],"runtime_image":runtime_info,"scene_image":manifest["image"],"rootfs":rootfs,"binaries":bin_inventory,"sources":source_inventory,"services":list(SERVICES),"deployment_order":["scene","camera-a","gas","audio","sensorlink-source","sensorlink-bridge","fusion"],"world_grants":[{"service":svc,"world":next(e for e in spec["services"][svc]["entitlements"] if e["type"]=="world")} for svc in SERVICES if svc in SENSORS or svc=="fusion"],"phases":["bootstrap_normal_acme_without_world","deploy_scene_only","verify_scene_clock_and_cni","commission_world_and_app_configs","initialize_world","restart_agent_preserving_scene","deploy_capture_services_individually","commission_sensorlink_source_cni","deploy_sensorlink_bridge","deploy_fusion","collect_allowlisted_receipts","cleanup_owned_resources"],"required_clock_limits":{"messages_per_second":200,"message_burst":400},"readiness":"detached CLI success is not application readiness","credential_policy":"generate only inside owned runtime; no credentials in project/build contexts or exported receipts"}
    plan["package_sha256"] = source_inventory["examples/live-office/world.json"]["sha256"]
    write_new(out/"plan.json",json_bytes(plan))
    files = inventory(out)
    write_new(out/"files.json",json_bytes({"version":1,"files":files}))
    validate_stage(out)
    return plan


def inventory(root):
    result = {}
    for path in sorted(root.rglob("*")):
        if path.is_symlink(): raise Refused("staging contains a symlink")
        if path.is_file():
            result[str(path.relative_to(root))] = {"sha256":digest_file(path),"size":path.stat().st_size,"mode":stat.S_IMODE(path.stat().st_mode)}
    return result


def validate_stage(root):
    root = Path(root)
    manifest = json.loads(regular(root/"files.json", 1 << 20).read_bytes())
    actual = inventory(root); actual.pop("files.json",None)
    if manifest.get("version") != 1 or manifest.get("files") != actual:
        raise Refused("staged inventory changed, gained files, or is incomplete")
    plan = json.loads((root/"plan.json").read_bytes())
    if plan.get("kind") != "wendy.agentmultimodal.plan.v1" or plan.get("simulation_only") is not True:
        raise Refused("explicit simulation staging plan required")
    validate_project(json.loads((root/"project/wendy.json").read_bytes()),plan["run_id"])
    if set(plan.get("binaries",{})) != set(BINARIES) or set(plan.get("sources",{})) != set(SOURCE_FILES):
        raise Refused("staging input inventory is incomplete")
    for name in ("agent","cli","initializer","fixture"):
        if elf_arm64(root/"tools"/name) != plan["binaries"][name]:
            raise Refused("staged tool differs from declared build artifact")
    for service in SERVICES:
        context=root/"project/services"/service
        expected=dockerfile(service,plan["run_id"],plan["anchor_unix_ns"],plan["seed"],plan["scene_image"]["environment"])
        if (context/"Dockerfile").read_bytes()!=expected or elf_arm64(context/"app-runner")!=plan["binaries"]["app-runner"]:
            raise Refused("staged service launcher changed")
        if service=="scene":
            if inspect_rootfs(context/"rootfs.tar")!=plan["rootfs"]:
                raise Refused("staged scene rootfs changed")
            for name,source in plan["sources"].items():
                p=context/name
                if digest_file(p)!=source["sha256"] or p.stat().st_size!=source["size"]:
                    raise Refused("staged scene source changed")
        else:
            binary="sensor" if service in ("camera-a","gas","audio") else "sensorlink" if service.startswith("sensorlink-") else "fusion"
            if elf_arm64(context/"app")!=plan["binaries"][binary]:
                raise Refused("staged app differs from declared build artifact")
    if plan.get("package_sha256")!=plan["sources"]["examples/live-office/world.json"]["sha256"]:
        raise Refused("planned package identity changed")
    return plan


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest="command",required=True)
    export=sub.add_parser("export-rootfs"); export.add_argument("--scene-image",required=True); export.add_argument("--output",type=Path,required=True)
    prep=sub.add_parser("prepare"); prep.add_argument("--base",type=Path,required=True); prep.add_argument("--output",type=Path,required=True); prep.add_argument("--runtime-image",required=True); prep.add_argument("--run-id",required=True); prep.add_argument("--anchor-unix-ns",type=int,required=True); prep.add_argument("--seed",type=int,default=7)
    for name in BINARIES: prep.add_argument("--"+name,type=Path,required=True)
    check=sub.add_parser("validate"); check.add_argument("stage",type=Path)
    args=parser.parse_args()
    try:
        if args.command=="export-rootfs": value=export_rootfs(args.scene_image,args.output)
        elif args.command=="prepare": value=prepare(args.base,args.output,args.runtime_image,{name:getattr(args,name.replace("-","_")) for name in BINARIES},args.run_id,args.anchor_unix_ns,args.seed)
        else: value=validate_stage(args.stage)
        print(json.dumps({"kind":value["kind"],"validated":True,"runtime_started":False}))
    except (Refused,OSError,ValueError,KeyError,subprocess.SubprocessError,tarfile.TarError) as error:
        parser.exit(1,"multimodal stage refused: "+str(error)+"\n")


if __name__=="__main__": main()
