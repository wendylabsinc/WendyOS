"""Prepare exact historical reads; this module never launches a runtime.

The saved fuser's locally_verified flag is a recorded claim, not a new signature
check. The normal proofreader validates its configuration and retrieves exact
historical bytes. Neither preparation nor historical retrieval proves current
authority of the original sources.

prepare accepts bounded raw JSON, keeping duplicate-field detection at its input
boundary. Commission fields come from independent enrollment/scene/deployment
receipts. Source generations stay pinned across both phases; only the reader's
generation changes. Inputs use the fixed service order below, followed by the
result. No signed JSON is reconstructed. write_phase creates a new local phase
directory. stdin_transfer returns argv and bytes for a caller's bounded exec;
it does not execute anything. JSON is never placed in an argument.
"""
import base64
from dataclasses import dataclass
import json
import os
from pathlib import Path, PurePosixPath

import receipts as r
from stage import APP, CAPTURE, RESULT, SENSORS, Refused

MAX_INPUT = MAX_CONFIG = 2 << 20
MAX_TOTAL_PROOFS = 1 << 20
ORDER = ("camera-a", "sensorlink-bridge", "gas", "audio", "fusion")
READER = "proof-reader"
PHASES = ("before_restart", "after_restart")


@dataclass(frozen=True)
class Prepared:
    config: bytes
    receipt: bytes


@dataclass(frozen=True)
class Transfer:
    argv: tuple
    stdin: bytes


def encode(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"),
                       ensure_ascii=False, allow_nan=False) + "\n").encode()


def text(value, limit=256):
    r.require(isinstance(value, str) and 0 < len(value.encode()) <= limit and
              value == value.strip() and not any(ord(c) < 32 or c in "*?" for c in value),
              "invalid commissioned text")
    return value


def commission(raw):
    c = r.shape(r.strict_json(raw, MAX_INPUT),
                ("version", "tenant", "site", "device", "app", "run_id", "frame",
                 "scene_boot_id", "package_sha256", "anchor_unix_ns",
                 "scenario_version", "seed", "publisher_generations"))
    r.integer(c["version"], 1, 1)
    r.require(c["app"] == APP, "wrong commissioned app/version")
    for key in ("tenant", "site", "device", "run_id", "scene_boot_id", "scenario_version"):
        text(c[key])
    r.shape(c["frame"], ("id", "revision"))
    for value in c["frame"].values(): text(value)
    r.digest(c["package_sha256"])
    r.integer(c["anchor_unix_ns"], 1)
    r.integer(c["seed"], 0, (1 << 32)-1)
    r.shape(c["publisher_generations"], ORDER)
    for value in c["publisher_generations"].values(): r.integer(value, 1, (1 << 64)-1)
    return c


def reader(raw):
    value = r.shape(r.strict_json(raw, MAX_INPUT),
                    ("phase", "app", "service", "expected_generation", "duration_ms"),
                    ("request_timeout_ms", "poll_interval_ms", "clock_max_width_ms"))
    r.require(value["phase"] in PHASES and value["app"] == APP and value["service"] == READER,
              "reader scope/phase differs")
    r.integer(value["expected_generation"], 1, (1 << 64)-1)
    r.integer(value["duration_ms"], 1, 60000)
    for key, default, lo, hi in (("request_timeout_ms", 1000, 10, 1000),
                                  ("poll_interval_ms", 500, 250, 5000),
                                  ("clock_max_width_ms", 5000, 1, 10000)):
        value.setdefault(key, default)
        r.integer(value[key], lo, hi)
    return value


def decode_proof(raw, service, c):
    """Check envelope structure and exact scope; do not claim signature validity."""
    signed = r.shape(r.strict_json(raw, r.MAX_PROOF), ("delegation", "record", "signature"))
    r.require(len(r.binary(signed["signature"], 64)) == 64, "record signature shape")
    inner = r.binary(signed["record"], 64 << 10)
    record = r.shape(r.strict_json(inner, 64 << 10),
                     ("version", "authority_domain", "delegation_sha256", "scope", "epoch",
                      "sequence", "issued_at_unix_ms", "expires_at_unix_ms", "publication"))
    component, stream = (RESULT, "hazard") if service == "fusion" else (CAPTURE, SENSORS[service])
    scope = {key: c[key] for key in ("tenant", "site", "device", "app")}
    scope.update(service=service, deployment_generation=c["publisher_generations"][service],
                 execution_domain="simulation", publish=[dict(component_type=component, stream=stream)])
    r.integer(record["version"], 1, 1)
    r.require(encode(record["scope"]) == encode(scope) and record["authority_domain"] == "physical",
              "proof scope differs from commissioned source")
    for key in ("epoch", "sequence"): r.integer(record[key], 1, (1 << 64)-1)
    r.integer(record["issued_at_unix_ms"], 1)
    r.integer(record["expires_at_unix_ms"], record["issued_at_unix_ms"]+1)
    delegation = r.shape(signed["delegation"], ("claims", "signature", "chain_der"))
    claims_raw = r.binary(delegation["claims"], 16 << 10)
    claims = r.shape(r.strict_json(claims_raw, 16 << 10),
                     ("version", "authority_domain", "scope", "epoch", "issued_at_unix_ms",
                      "expires_at_unix_ms", "public_key"))
    r.integer(claims["version"], 1, 1)
    r.integer(claims["epoch"], 1, (1 << 64)-1)
    r.require(r.sha(claims_raw) == record["delegation_sha256"] and
              claims["authority_domain"] == "physical" and encode(claims["scope"]) == encode(scope) and
              claims["epoch"] == record["epoch"], "delegation differs from record")
    r.integer(claims["issued_at_unix_ms"], 1, record["issued_at_unix_ms"])
    r.integer(claims["expires_at_unix_ms"], record["expires_at_unix_ms"])
    r.require(len(r.binary(claims["public_key"], 32)) == 32 and
              len(r.binary(delegation["signature"], 3309)) == 3309, "delegation key/signature shape")
    r.require(isinstance(delegation["chain_der"], list) and 1 <= len(delegation["chain_der"]) <= 8,
              "certificate chain bound")
    for cert in delegation["chain_der"]: r.binary(cert, 32 << 10)
    p = r.shape(record["publication"], ("entity", "component_type", "stream", "frame", "timing",
                                       "ttl_ns", "provenance", "payload", "tombstone"))
    r.require(p["component_type"] == component and p["stream"] == stream and p["frame"] == c["frame"] and
              p["tombstone"] is False, "publication scope/frame differs")
    text(p["entity"])
    r.integer(p["ttl_ns"], 1, 24*3600*10**9)
    timing = r.shape(p["timing"], ("sample_time", "clock_domain", "error_known", "error_bound_ns", "age_hint_ns"))
    r.timestamp(timing["sample_time"])
    r.require(timing["clock_domain"] == "simulation:"+c["run_id"] and timing["error_known"] is True,
              "publication source clock differs")
    for key in ("error_bound_ns", "age_hint_ns"): r.integer(timing[key], 0, p["ttl_ns"])
    source = {key: scope[key] for key in ("device", "app", "service")}
    source["stream"] = stream
    ref = dict(key=dict(tenant=c["tenant"], site=c["site"], entity=p["entity"], type=component, source=source),
               version=dict(epoch=record["epoch"], sequence=record["sequence"]),
               envelope_sha256=r.sha(raw), record_sha256=r.sha(inner))
    return dict(raw=raw, record=record, publication=p, reference=ref,
                payload=r.strict_json(r.binary(p["payload"], 32 << 10), 32 << 10))


def capture(item, service, c):
    p, payload = item["publication"], item["payload"]
    sample = r.capture(payload, c, {"boot_id": c["scene_boot_id"]}, SENSORS[service])
    r.integer(payload["version"], 1, 1)
    r.integer(sample["seed"], 0, (1 << 32)-1)
    kind = sample["kind"]
    fields = {"camera": ("width", "height", "fx", "fy", "cx", "cy", "camera_to_site", "calibration_valid_for_ns"),
              "audio": ("sample_rate_hz", "channels", "duration_ns"),
              "gas": ("value_ppm", "uncertainty_ppm", "species")}
    r.shape(sample[kind], fields[kind])
    r.require(p["entity"] == "sensor/"+sample["sensor_id"] and
              r.timestamp(p["timing"]["sample_time"]) == c["anchor_unix_ns"]+sample["sample_ns"],
              "capture source time/entity changed")
    r.require(p["provenance"] == dict(origin="simulated", app_version="world-sensor-capture-v1",
              model_id=sample["sensor_model"], model_version=sample["scenario_version"], simulation=r.simulation(c)),
              "raw capture provenance changed")
    r.require(("sensorlink" in payload) == (service == "sensorlink-bridge"), "unexpected SensorLink binding")
    media = sample.get("evidence")
    if service == "sensorlink-bridge":
        wire = r.shape(payload["sensorlink"], ("protocol", "channel_id", "frame_sequence", "timestamp_us", "flags",
                         "scene_boot_id", "capture_sequence", "captured_unix_ns", "sample_sha256", "calibration_sha256", "evidence"))
        r.require(wire["protocol"] == "wendylite.v1.sensorlink" and wire["scene_boot_id"] == payload["scene_boot_id"] and
                  wire["capture_sequence"] == sample["sequence"] and wire["captured_unix_ns"] == payload["captured_unix_ns"] and
                  payload["captured_unix_ns"] % 1000 == 0 and wire["timestamp_us"] == payload["captured_unix_ns"]//1000 and
                  wire["sample_sha256"] == r.sha(r.binary(payload["sample_json"], 32 << 10)), "wire capture binding differs")
        for key in ("channel_id", "frame_sequence"): r.integer(wire[key], 1, (1 << 32)-1)
        r.integer(wire["flags"], 0, 1)
        r.digest(wire["calibration_sha256"])
        media = r.evidence(wire["evidence"])
        r.require(media["content_type"] == "image/jpeg" and sample["evidence"]["content_type"] == "image/png",
                  "wire JPEG/original PNG differs")
    item.update(sample=sample, capture=payload, media=media)


def hazard_shapes(out):
    evaluation = r.shape(out["evaluation"], ("state", "reason", "input_digest", "hazard"))
    h = r.shape(evaluation["hazard"], ("label", "confidence", "confidence_meaning", "position_m", "uncertainty_m",
                 "location_basis", "gas_value_ppm", "gas_uncertainty_ppm", "gas_species", "cameras", "audio",
                 "inputs", "evidence", "valid_until", "input_digest"))
    for camera in h["cameras"]: r.shape(camera, ("sensor_id", "red_fraction"))
    r.shape(h["audio"], ("rms", "tone_fraction"))
    for item in h["inputs"]:
        r.shape(item, ("sample", "sample_id", "sensor_id", "kind", "source", "version", "sample_time",
                       "capture_start", "valid_until", "capture"), ("evidence",))
        r.shape(item["capture"], ("proof_sha256", "scene_boot_id", "simulation_anchor"), ("original_evidence",))
        r.shape(item["version"], ("epoch", "sequence"))
        for number in item["version"].values(): r.integer(number, 1, (1 << 64)-1)
    return h


def reference_shape(ref):
    r.shape(ref, ("key", "version", "envelope_sha256", "record_sha256"))
    key = r.shape(ref["key"], ("tenant", "site", "entity", "type", "source"))
    r.shape(key["source"], ("device", "app", "service", "stream"))
    for name in ("tenant", "site", "entity", "type"): text(key[name])
    for value in key["source"].values(): text(value)
    r.shape(ref["version"], ("epoch", "sequence"))
    for value in ref["version"].values(): r.integer(value, 1, (1 << 64)-1)
    for name in ("envelope_sha256", "record_sha256"): r.digest(ref[name])


def _prepare(commission_raw, stdout_raw, exit_raw, reader_raw, previous):
    c, target = commission(commission_raw), reader(reader_raw)
    out, exit_value = r.strict_json(stdout_raw, MAX_INPUT), r.strict_json(exit_raw, 4096)
    r.wrapper({"exit.json": exit_value, "stdout.json": out})
    r.require(exit_value["stdout_bytes"] == len(stdout_raw), "wrapper stdout count differs from original bytes")
    r.shape(out, ("version", "run_id", "status", "attempts", "evaluation", "inputs", "media", "request_json",
                  "signed_record_json", "record_sha256", "proof_sha256", "locally_verified"))
    r.integer(out["version"], 1, 1)
    r.require(out["version"] == 1 and out["status"] == "complete" and out["locally_verified"] is True and
              out["run_id"] == c["run_id"], "successful locally verified fusion required")
    r.require(isinstance(out["inputs"], list) and len(out["inputs"]) == 4, "exact four input proofs required")
    by_service, total = {}, 0
    for entry in out["inputs"]:
        r.shape(entry, ("reference", "envelope_json"))
        reference_shape(entry["reference"])
        raw = r.binary(entry["envelope_json"], r.MAX_PROOF)
        service = entry["reference"]["key"]["source"]["service"]
        r.require(service in SENSORS and service not in by_service, "duplicate or unexpected capture source")
        item = decode_proof(raw, service, c)
        r.require(entry["reference"] == item["reference"], "input reference differs from original envelope")
        capture(item, service, c)
        by_service[service] = item
        total += len(raw)
    r.require(set(by_service) == set(SENSORS), "required capture source missing")
    ack = decode_proof(r.binary(out["signed_record_json"], r.MAX_PROOF), "fusion", c)
    r.shape(ack["payload"], ("version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"))
    r.integer(ack["payload"]["version"], 1, 1)
    for ref in ack["payload"]["inputs"]: reference_shape(ref)
    total += len(ack["raw"])
    r.require(total <= MAX_TOTAL_PROOFS, "embedded proof budget exceeded")
    hazard_shapes(out)
    states = {key: dict(generation=value) for key, value in c["publisher_generations"].items()}
    # Reuse the exact result/request, media-byte, lifetime and lineage checks.
    # by_hash comes from the saved inputs here, not separate publisher receipts;
    # preparation therefore makes no independent publisher-acknowledgment claim.
    r.validate_fusion(out, {r.sha(x["raw"]): x for x in by_service.values()}, c,
                      {"boot_id": c["scene_boot_id"]}, states,
                      {key: [value] for key, value in by_service.items()}, 0)
    by_service["fusion"] = ack
    expected = [dict(reference=by_service[s]["reference"],
                     envelope_json=base64.b64encode(by_service[s]["raw"]).decode()) for s in ORDER]
    media = []
    for service in ORDER[:-1]:
        item = by_service[service]
        if item["media"] is not None:
            row = dict(source=dict(app=APP, service=service, component_type=CAPTURE, stream=SENSORS[service]),
                       reference=item["media"], capture_proof_sha256=item["reference"]["envelope_sha256"])
            if service == "sensorlink-bridge": row["original_evidence"] = item["sample"]["evidence"]
            media.append(row)
    config = dict(version=1, **{key: c[key] for key in ("tenant", "site", "device", "app", "run_id")},
                  **{key: target[key] for key in ("service", "expected_generation", "duration_ms",
                                                  "request_timeout_ms", "poll_interval_ms", "clock_max_width_ms")},
                  expected=expected)
    raw_config = encode(config)
    r.require(len(raw_config) <= MAX_CONFIG, "reader config exceeds byte budget")
    receipt = dict(version=1, kind="wendy.agentmultimodal.history.phase.v1", phase=target["phase"],
                   config_sha256=r.sha(raw_config), commission_sha256=r.sha(encode(c)),
                   fusion_stdout_sha256=r.sha(stdout_raw), fusion_exit_sha256=r.sha(exit_raw),
                   proof_bundle_sha256=r.sha(encode(expected)), reader={key: config[key] for key in
                   ("tenant", "site", "device", "app", "service", "expected_generation", "run_id")},
                   references=[x["reference"] for x in expected], media=media,
                   retrieval_verified=False, current_source_authority=False)
    if previous is None:
        r.require(target["phase"] == PHASES[0], "after-restart phase needs original phase")
    else:
        old_config, old = validate_prepared(previous)
        r.require(target["phase"] == PHASES[1] and old["phase"] == PHASES[0], "phase order differs")
        for key in ("commission_sha256", "fusion_stdout_sha256", "fusion_exit_sha256", "proof_bundle_sha256", "references", "media"):
            r.require(receipt[key] == old[key], "historical source bundle changed across reader generations")
        r.require(config["expected"] == old_config["expected"], "proof order or bytes changed")
        r.require(config["expected_generation"] != old_config["expected_generation"], "fresh reader generation required")
        r.require({k: v for k, v in receipt["reader"].items() if k != "expected_generation"} ==
                  {k: v for k, v in old["reader"].items() if k != "expected_generation"}, "reader scope changed across phases")
    return Prepared(raw_config, encode(receipt))


def prepare(commission_json, fusion_stdout, wrapper_exit, reader_json, previous=None):
    """Return immutable config/receipt bytes without writing files or doing I/O."""
    try:
        return _prepare(commission_json, fusion_stdout, wrapper_exit, reader_json, previous)
    except Refused:
        raise
    except (KeyError, TypeError, ValueError, OverflowError, RecursionError) as error:
        raise Refused("malformed historical preparation input") from error


def _validate_prepared(prepared):
    r.require(isinstance(prepared, Prepared), "prepared phase required")
    c = r.shape(r.strict_json(prepared.config, MAX_CONFIG),
                ("version", "tenant", "site", "device", "app", "service", "expected_generation", "run_id",
                 "duration_ms", "request_timeout_ms", "poll_interval_ms", "clock_max_width_ms", "expected"))
    p = r.shape(r.strict_json(prepared.receipt, MAX_CONFIG),
                ("version", "kind", "phase", "config_sha256", "commission_sha256", "fusion_stdout_sha256",
                 "fusion_exit_sha256", "proof_bundle_sha256", "reader", "references", "media",
                 "retrieval_verified", "current_source_authority"))
    r.integer(c["version"], 1, 1)
    r.integer(p["version"], 1, 1)
    reader(encode({"phase": p["phase"], **{key: c[key] for key in
                  ("app", "service", "expected_generation", "duration_ms", "request_timeout_ms", "poll_interval_ms", "clock_max_width_ms")}}))
    r.require(c["version"] == p["version"] == 1 and p["kind"] == "wendy.agentmultimodal.history.phase.v1" and
              p["phase"] in PHASES and p["retrieval_verified"] is False and p["current_source_authority"] is False,
              "phase receipt kind/status differs")
    for key in ("config_sha256", "commission_sha256", "fusion_stdout_sha256", "fusion_exit_sha256", "proof_bundle_sha256"):
        r.digest(p[key])
    r.require(p["config_sha256"] == r.sha(prepared.config) and p["proof_bundle_sha256"] == r.sha(encode(c["expected"])) and
              p["reader"] == {key: c[key] for key in ("tenant", "site", "device", "app", "service", "expected_generation", "run_id")},
              "phase config/generation differs from receipt")
    r.require(isinstance(c["expected"], list) and len(c["expected"]) == 5 and
              p["references"] == [x["reference"] for x in c["expected"]], "phase references differ")
    total = 0
    derived_media = []
    for service, entry in zip(ORDER, c["expected"]):
        r.shape(entry, ("reference", "envelope_json"))
        reference_shape(entry["reference"])
        raw = r.binary(entry["envelope_json"], r.MAX_PROOF)
        total += len(raw)
        signed = r.shape(r.strict_json(raw, r.MAX_PROOF), ("delegation", "record", "signature"))
        inner = r.binary(signed["record"], 64 << 10)
        record = r.strict_json(inner, 64 << 10)
        scope = record["scope"]
        r.integer(scope["deployment_generation"], 1, (1 << 64)-1)
        context = {key: c[key] for key in ("tenant", "site", "device", "app", "run_id")}
        context.update(frame=record["publication"]["frame"], publisher_generations={service: scope["deployment_generation"]})
        item = decode_proof(raw, service, context)
        r.require(item["reference"] == entry["reference"], "prepared proof reference differs")
        if service == "fusion":
            r.shape(item["payload"], ("version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"))
            for ref in item["payload"]["inputs"]: reference_shape(ref)
            r.require(len(item["payload"]["inputs"]) == 4 and
                      {encode(ref) for ref in item["payload"]["inputs"]} == {encode(x["reference"]) for x in c["expected"][:-1]},
                      "prepared result inputs differ")
            continue
        sample = r.strict_json(r.binary(item["payload"]["sample_json"], 32 << 10), 32 << 10)
        context.update(scene_boot_id=item["payload"]["scene_boot_id"], anchor_unix_ns=item["payload"]["simulation_anchor_unix_ns"],
                       package_sha256=sample["package_sha256"], scenario_version=sample["scenario_version"], seed=sample["seed"])
        capture(item, service, context)
        if item["media"] is not None:
            row = dict(source=dict(app=APP, service=service, component_type=CAPTURE, stream=SENSORS[service]),
                       reference=item["media"], capture_proof_sha256=item["reference"]["envelope_sha256"])
            if service == "sensorlink-bridge": row["original_evidence"] = item["sample"]["evidence"]
            derived_media.append(row)
    r.require(total <= MAX_TOTAL_PROOFS and p["media"] == derived_media, "prepared media is not bound to original proofs")
    return c, p


def validate_prepared(prepared):
    """Reject swapped config/receipt pairs before installation or phase reuse.

    This checks the caller's saved artifact, not independent commissioning or
    signatures. Keep the returned pair in an owned directory. Recomputed local
    hashes cannot establish trust in an untrusted replacement artifact.
    """
    try:
        return _validate_prepared(prepared)
    except Refused:
        raise
    except (KeyError, TypeError, ValueError, OverflowError, RecursionError) as error:
        raise Refused("malformed prepared phase") from error


def _directory(path):
    """Open an existing owned directory through no-follow directory descriptors."""
    path = os.fspath(path)
    r.require(path.startswith("/") and str(PurePosixPath(path)) == path and ".." not in PurePosixPath(path).parts,
              "clean absolute directory required")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in PurePosixPath(path).parts[1:]:
            new = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = new
        st = os.fstat(fd)
        r.require(st.st_uid == os.geteuid() and not st.st_mode & 0o022, "owned non-writable-by-others directory required")
        return fd
    except BaseException:
        os.close(fd)
        raise


def _write_new(directory, name, data):
    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
    with os.fdopen(fd, "wb") as out:
        out.write(data)
        out.flush()
        os.fsync(out.fileno())


def write_phase(parent, prepared):
    """Create config.json then preparation.json under a NEW phase directory.

    A failed/partial phase stays in place and cannot be reused. No old receipts
    are replaced or deleted. The caller chooses an owned private local parent.
    """
    _, receipt = validate_prepared(prepared)
    fd = _directory(parent)
    try:
        phase = receipt["phase"]
        os.mkdir(phase, 0o700, dir_fd=fd)
        child = os.open(phase, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
        try:
            _write_new(child, "config.json", prepared.config)
            _write_new(child, "preparation.json", prepared.receipt)
            os.fsync(child)
        finally:
            os.close(child)
        os.fsync(fd)
    finally:
        os.close(fd)
    return Path(parent) / phase


# Standalone receiver for an explicitly invoked docker-exec stdin transfer.
# Ten seconds bounds stdin; the caller must also bound total exec duration.
# No runtime, Docker or file mutation happens merely by requesting this command.
STDIN_RECEIVER = r'''
import hashlib, os, pathlib, select, stat, sys, time
def require(value, reason):
    if not value: raise ValueError(reason)
path, wanted, length = sys.argv[1:]
length = int(length)
require(0 < length <= 2 << 20, 'byte budget')
p = pathlib.PurePosixPath(path)
require(p.is_absolute() and str(p) == path and '..' not in p.parts and p.name == 'config.json', 'path')
data = bytearray(); deadline = time.monotonic() + 10
while True:
    remaining = deadline - time.monotonic()
    require(remaining > 0 and select.select([0], [], [], remaining)[0], 'stdin deadline')
    block = os.read(0, min(65536, length + 1 - len(data)))
    if not block: break
    data.extend(block)
    require(len(data) <= length, 'stdin size')
require(len(data) == length and hashlib.sha256(data).hexdigest() == wanted, 'stdin digest/length')
fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
try:
    for part in p.parts[1:-1]:
        nextfd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
        os.close(fd); fd = nextfd
    st = os.fstat(fd)
    require(st.st_uid == os.geteuid() and not st.st_mode & 0o022, 'directory ownership')
    outfd = os.open(p.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
    with os.fdopen(outfd, 'wb') as out:
        out.write(data); out.flush(); os.fsync(out.fileno())
    os.fsync(fd)
finally:
    os.close(fd)
'''


def stdin_transfer(prepared, destination):
    """Return bounded exact stdin and small argv for an owned config volume.

    Root must prepend its explicit owned-container exec command with stdin open,
    enforce a total timeout and inspect the exit status. A nonzero/unknown result
    is not retried or treated as successful installation. The receiver refuses
    existing targets, symlinks, different hashes, truncation and oversized stdin.
    """
    validate_prepared(prepared)
    path = PurePosixPath(destination)
    r.require(str(path) == destination and path.is_absolute() and ".." not in path.parts and
              path.name == "config.json" and len(path.parts) >= 7 and
              path.parts[:5] == ("/", "var", "lib", "wendy", "volumes"), "owned config volume path required")
    return Transfer(("python3", "-c", STDIN_RECEIVER, destination, r.sha(prepared.config), str(len(prepared.config))),
                    prepared.config)


def retain_output(phase_directory, exit_bytes, stdout_bytes, stderr_bytes):
    """Keep bounded exact reader outputs, including failed or empty stdout.

    This records bytes and no success assertion. A later validator must check
    the actual reader result. Partial output sets remain non-overwritable.
    """
    values = {"reader-exit.json": (exit_bytes, 4096), "reader-stdout.json": (stdout_bytes, 32 << 20),
              "reader-stderr.log": (stderr_bytes, 64 << 10)}
    for value, limit in values.values():
        r.require(isinstance(value, bytes) and len(value) <= limit, "reader output exceeds byte bound")
    fd = _directory(phase_directory)
    try:
        for name in (*values, "output-manifest.json"):
            try: os.stat(name, dir_fd=fd, follow_symlinks=False)
            except FileNotFoundError: continue
            raise Refused("phase output already exists")
        manifest = {}
        for name, (data, _) in values.items():
            _write_new(fd, name, data)
            manifest[name] = dict(size=len(data), sha256=r.sha(data))
        _write_new(fd, "output-manifest.json", encode(dict(version=1, files=manifest, retrieval_verified=False)))
        os.fsync(fd)
    finally:
        os.close(fd)
