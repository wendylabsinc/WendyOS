"""Pure consistency checks for saved ordinary historical-reader receipts.

The caller must retain exit/stdout/stderr bytes before validation, including on
failure, and supply the independently observed reader task and scene. This
module performs no I/O, RPC, signature verification or clock authentication.
The app receipt has no deployment identity and no raw Query reply: identity is
bound through Prepared and the task observation, while original-version absence
remains a reader-reported result. Saved historical proofs never establish current
source authority. Hashes cannot authenticate a replaced commissioning artifact.
"""
import copy
import ipaddress
from pathlib import PurePosixPath
from urllib.parse import urlsplit

import history as h
import receipts as r
from stage import APP, CAPTURE, SENSORS, Refused

MAX_EXIT = 4096
MAX_STDERR = 64 << 10
MAX_MEDIA = 16 << 20
MAX_PHYSICAL_AGE_NS = 5_000_000_000
MAX_NS = (1 << 63)-1
TASK_FIELDS = ("container", "app", "service", "pid", "start_ticks", "netns_inode",
               "cgroup", "ipv4", "world_socket")


def _path(value):
    h.text(value, 4096)
    p = PurePosixPath(value)
    r.require(p.is_absolute() and not value.startswith("//") and str(p) == value and ".." not in p.parts and value != "/",
              "clean absolute observed path required")


def _cgroup(value):
    # Runner.task preserves the stripped /proc/PID/cgroup file, not just its
    # path. Require a single unified-v2 line; never reinterpret v1/mixed output.
    h.text(value, 4096)
    fields = value.split(":", 2)
    r.require(len(fields) == 3 and fields[:2] == ["0", ""], "one cgroup v2 observation required")
    _path(fields[2])


def _task(task, service, world):
    r.shape(task, TASK_FIELDS + (("generation", "socket_directory") if world else ()))
    r.require(task["container"] == APP+"_"+service and task["app"] == APP and
              task["service"] == service and task["world_socket"] is world,
              "observed task scope differs")
    for key in ("pid", "start_ticks", "netns_inode"):
        r.integer(task[key], 2)
    _cgroup(task["cgroup"])
    address = ipaddress.IPv4Address(task["ipv4"])
    r.require(str(address) == task["ipv4"], "canonical task IPv4 required")
    if world:
        r.integer(task["generation"], 1, (1 << 64)-1)
        _path(task["socket_directory"])
    return address


def _observations(config, task, scene):
    """Bind supplied observations, without pretending to reacquire their trust."""
    # Copy only after bounding: callers keep their exact independent observations.
    r.bounded(task)
    r.bounded(scene)
    task, scene = copy.deepcopy(task), copy.deepcopy(scene)
    r.shape(scene, ("version", "kind", "scene_url", "site", "app_id", "run_id", "boot_id",
                    "package_sha256", "frame", "anchor_unix_ns", "clock_domain",
                    "simulation_time_ns", "observed_unix_ns", "observation_sequence", "task", "app_subnet"))
    r.integer(scene["version"], 1, 1)
    r.require(scene["kind"] == "wendy.agentmultimodal.scene.v1" and scene["app_id"] == APP and
              scene["site"] == config["site"] and scene["run_id"] == config["run_id"] and
              scene["clock_domain"] == "simulation:"+config["run_id"], "observed scene scope differs")
    h.text(scene["boot_id"])
    r.digest(scene["package_sha256"])
    r.shape(scene["frame"], ("id", "revision"))
    for value in scene["frame"].values():
        h.text(value)
    for key in ("anchor_unix_ns", "observed_unix_ns", "observation_sequence"):
        r.integer(scene[key], 1)
    r.integer(scene["simulation_time_ns"], 0, MAX_NS-scene["anchor_unix_ns"]-1)
    scene_address = _task(scene["task"], "scene", False)
    reader_address = _task(task, h.READER, True)
    r.require(task["generation"] == config["expected_generation"], "reader deployment generation differs")
    network = ipaddress.IPv4Network(scene["app_subnet"], strict=True)
    r.require(str(network) == scene["app_subnet"] and network.prefixlen == 28,
              "observed owned app subnet differs")
    for address in (scene_address, reader_address):
        r.require(address in network and address not in (network.network_address, network.broadcast_address),
                  "observed task outside app subnet")
    r.require(reader_address != scene_address and all(task[key] != scene["task"][key]
              for key in ("pid", "netns_inode", "cgroup")), "reader and scene task identities overlap")
    endpoint = urlsplit(scene["scene_url"])
    r.require(endpoint.scheme == "http" and endpoint.hostname == str(scene_address) and endpoint.port == 8091 and
              not any((endpoint.username, endpoint.password, endpoint.path, endpoint.query, endpoint.fragment)) and
              scene["scene_url"] == "http://"+str(scene_address)+":8091", "observed scene endpoint differs")
    return task, scene


def _originals(config, scene):
    """Decode original bytes to bind capture context and derive media owners."""
    context = {key: config[key] for key in ("tenant", "site", "device", "app", "run_id")}
    context.update(frame=scene["frame"], scene_boot_id=scene["boot_id"],
                   anchor_unix_ns=scene["anchor_unix_ns"], package_sha256=scene["package_sha256"],
                   publisher_generations={})
    expired_after, media = 0, []
    for service, expected in zip(h.ORDER, config["expected"]):
        envelope = r.binary(expected["envelope_json"], r.MAX_PROOF)
        signed = r.strict_json(envelope, r.MAX_PROOF)
        record = r.strict_json(r.binary(signed["record"], 64 << 10), 64 << 10)
        context["publisher_generations"][service] = record["scope"]["deployment_generation"]
        item = h.decode_proof(envelope, service, context)
        r.require(item["reference"] == expected["reference"], "original proof reference differs")
        if service == "fusion":
            continue
        sample = r.strict_json(r.binary(item["payload"]["sample_json"], 32 << 10), 32 << 10)
        if "scenario_version" not in context:
            context.update(scenario_version=sample["scenario_version"], seed=sample["seed"])
        h.capture(item, service, context)
        captured = r.integer(item["capture"]["captured_unix_ns"], 1, MAX_NS-MAX_PHYSICAL_AGE_NS)
        expired_after = max(expired_after, captured+MAX_PHYSICAL_AGE_NS)
        if item["media"] is not None:
            media.append(dict(source=dict(app=APP, service=service, component_type=CAPTURE, stream=SENSORS[service]),
                              reference=item["media"], capture_proof_sha256=expected["reference"]["envelope_sha256"]))
    r.require(len(media) == 3, "three original owned media references required")
    return expired_after, media


def _retention(row, latest):
    until = r.integer(row["retain_until_unix_ms"], 1, MAX_NS//1_000_000)
    r.require(latest < until*1_000_000, "reported final clock reaches original retention deadline")
    return until


def _validate(prepared, exit_bytes, stdout_bytes, stderr_bytes, reader_task, scene):
    config, phase = h.validate_prepared(prepared)
    task, scene = _observations(config, reader_task, scene)
    expired_after, media = _originals(config, scene)
    r.require(isinstance(stderr_bytes, bytes) and len(stderr_bytes) <= MAX_STDERR,
              "stderr byte budget exceeded")
    exit_value = r.strict_json(exit_bytes, MAX_EXIT)
    out = r.strict_json(stdout_bytes, r.MAX_APP)
    r.wrapper({"exit.json": exit_value, "stdout.json": out})
    r.integer(exit_value["version"], 1, 1)
    r.integer(exit_value["exit_code"], 0, 0)
    for field in ("signal", "forwarded_signal"):
        if field in exit_value:
            r.integer(exit_value[field], 0, 0)
    r.require(exit_value["stdout_bytes"] == len(stdout_bytes) and exit_value["stderr_bytes"] == len(stderr_bytes),
              "wrapper byte counts differ from retained outputs")
    r.shape(out, ("version", "status", "proofs", "media", "physical_capture_expired", "original_versions_absent",
                  "current_source_authority", "final_physical_earliest_unix_ns", "final_physical_latest_unix_ns"))
    r.integer(out["version"], 1, 1)
    r.require(out["status"] == "complete" and out["physical_capture_expired"] is True and
              out["original_versions_absent"] is True and out["current_source_authority"] is False,
              "reader did not report complete historical retrieval and original live absence")
    earliest = r.integer(out["final_physical_earliest_unix_ns"], 1)
    latest = r.integer(out["final_physical_latest_unix_ns"], earliest)
    r.require(latest-earliest <= config["clock_max_width_ms"]*1_000_000 and earliest > expired_after,
              "reported interval does not establish original capture expiry within configured width")
    r.require(isinstance(out["proofs"], list) and len(out["proofs"]) == 5, "five original proof receipts required")
    proof_summary = []
    for expected, got in zip(config["expected"], out["proofs"]):
        r.shape(got, ("reference", "envelope_json", "retain_until_unix_ms", "historical"))
        h.reference_shape(got["reference"])
        envelope = r.binary(got["envelope_json"], r.MAX_PROOF)
        r.require(got["historical"] is True and got["reference"] == expected["reference"] and
                  envelope == r.binary(expected["envelope_json"], r.MAX_PROOF), "historical original bytes/order differ")
        proof_summary.append(dict(reference=copy.deepcopy(expected["reference"]), envelope_bytes=len(envelope),
                                  retain_until_unix_ms=_retention(got, latest)))
    r.require(isinstance(out["media"], list) and len(out["media"]) == 3, "three original media receipts required")
    total_media, media_summary = 0, []
    for expected, got in zip(media, out["media"]):
        r.shape(got, ("source", "reference", "data", "retain_until_unix_ms"))
        r.shape(got["source"], ("app", "service", "component_type", "stream"))
        r.evidence(got["reference"])
        r.require(got["source"] == expected["source"] and got["reference"] == expected["reference"],
                  "historical media owner/reference/order differs from original capture")
        data = r.binary(got["data"], 8 << 20)
        total_media += len(data)
        r.require(total_media <= MAX_MEDIA and len(data) == got["reference"]["size"] and
                  r.sha(data) == got["reference"]["sha256"], "historical media bytes differ from original digest/size")
        media_summary.append(dict(**copy.deepcopy(expected), retain_until_unix_ms=_retention(got, latest)))
    return dict(version=1, kind="wendy.agentmultimodal.history.receipts.v1", phase=phase["phase"],
                run_id=config["run_id"], reader=copy.deepcopy(phase["reader"]), reader_task=task,
                scene_sha256=r.sha(h.encode(scene)), config_sha256=r.sha(prepared.config),
                preparation_sha256=r.sha(prepared.receipt), proof_bundle_sha256=phase["proof_bundle_sha256"],
                outputs={name: dict(bytes=len(data), sha256=r.sha(data)) for name, data in
                         (("exit.json", exit_bytes), ("stdout.json", stdout_bytes), ("stderr.log", stderr_bytes))},
                proofs=proof_summary, media=media_summary, media_bytes=total_media,
                reported_final_interval=dict(earliest_unix_ns=earliest, latest_unix_ns=latest),
                original_capture_expired_after_unix_ns=expired_after, saved_receipt_consistency=True,
                reader_reported_historical_retrieval=True, reader_reported_physical_capture_expired=True,
                reader_reported_original_versions_absent=True, current_source_authority=False,
                independent_cryptographic_verification=False, independent_clock_authentication=False,
                independent_live_query=False,
                qualification="Run and reader generation are bound to Prepared and the supplied independent task/scene observations. "
                "Exact proof/media hashes and reported time bounds are saved-receipt consistency checks. "
                "Authenticated retrieval and original-version live absence are reader-reported; no raw Query reply is retained. "
                "Python does not authenticate clocks or proofs anew or establish current source authority.")


def validate_history_receipts(prepared, exit_bytes, stdout_bytes, stderr_bytes, reader_task, scene):
    """Return a bounded JSON-safe summary or refuse, leaving all inputs intact.

    The runner owns retaining *all* original bytes before calling, including
    failed wrappers and malformed/truncated output. This function neither saves
    nor discards them. Task observations are an input trust boundary, not an
    identity inferred from a source IP or receipt field.
    """
    try:
        return _validate(prepared, exit_bytes, stdout_bytes, stderr_bytes, reader_task, scene)
    except Refused:
        raise
    except (KeyError, TypeError, ValueError, OverflowError, RecursionError, UnicodeError, AttributeError) as error:
        raise Refused("malformed or inconsistent historical receipts") from error
