"""Bounded saved-receipt consistency checks, not a signature/authority verifier.

The ordinary fusion app verifies commissioned signatures while running. These
checks compare its retained bytes with the publishers and runtime commissioning.
Saved receipts establish neither current authority nor retained-proof retrieval.
"""
import base64
import binascii
import calendar
import copy
import datetime
import hashlib
import json
import math
import re

from stage import APP, CAPTURE, RESULT, SERVICES, SENSORS, Refused

MAX_APP = 32 << 20
MAX_PROOF = 256 << 10


def require(value, reason):
    if not value:
        raise Refused(reason)


def integer(value, minimum=0, maximum=(1 << 63)-1):
    require(type(value) is int and minimum <= value <= maximum, "integer outside receipt bound")
    return value


def shape(value, required, optional=()):
    require(isinstance(value, dict) and set(required) <= value.keys() <= set(required) | set(optional), "unexpected receipt fields")
    return value


def strict_json(raw, limit):
    require(isinstance(raw, bytes) and 0 < len(raw) <= limit, "JSON byte budget exceeded")
    def pairs(items):
        out = {}
        for key, value in items:
            require(key not in out, "duplicate JSON field")
            out[key] = value
        return out
    try:
        out = json.loads(raw.decode("utf-8"), object_pairs_hook=pairs,
                         parse_constant=lambda _: (_ for _ in ()).throw(Refused("nonfinite JSON")))
        bounded(out)
        return out
    except (UnicodeError, ValueError, RecursionError) as error:
        raise Refused("invalid bounded JSON") from error


def bounded(value):
    pending = [(value, 0)]
    count = 0
    while pending:
        item, depth = pending.pop()
        count += 1
        require(count <= 200000 and depth <= 40, "JSON structural budget exceeded")
        if isinstance(item, dict):
            require(all(isinstance(k, str) for k in item), "JSON keys required")
            pending.extend((v, depth+1) for v in item.values())
            pending.extend((k, depth+1) for k in item)
        elif isinstance(item, list):
            pending.extend((v, depth+1) for v in item)
        elif isinstance(item, str):
            require(len(item) <= MAX_APP and not any(0xd800 <= ord(c) <= 0xdfff for c in item), "invalid JSON text")
        elif isinstance(item, float):
            require(math.isfinite(item), "nonfinite JSON")
        else:
            require(item is None or type(item) in (int, bool), "non-JSON receipt value")


def binary(value, limit):
    require(isinstance(value, str) and len(value) <= 4*((limit+2)//3), "base64 byte budget exceeded")
    try:
        raw = base64.b64decode(value, validate=True)
    except (ValueError, binascii.Error) as error:
        raise Refused("invalid base64 bytes") from error
    require(0 < len(raw) <= limit and base64.b64encode(raw).decode() == value, "noncanonical or empty byte field")
    return raw


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def digest(value):
    require(isinstance(value, str) and re.fullmatch(r"[a-f0-9]{64}", value), "invalid digest")
    return value


def timestamp(value):
    require(isinstance(value, str), "timestamp required")
    match = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z", value)
    require(match, "UTC nanosecond timestamp required")
    try:
        seconds = calendar.timegm(datetime.datetime.strptime(match[1], "%Y-%m-%dT%H:%M:%S").timetuple())
    except ValueError as error:
        raise Refused("invalid timestamp") from error
    return integer(seconds*10**9+int((match[2] or "").ljust(9,"0")), 1)


def evidence(ref):
    shape(ref, ("sha256", "content_type", "size"))
    digest(ref["sha256"])
    integer(ref["size"], 1, 8 << 20)
    require(ref["content_type"] in ("image/png", "image/jpeg", "audio/wav"), "unexpected evidence format")
    return ref


def proto_publication(publication):
    """Expected UseProtoNames JSON shape from the submitted Go publication."""
    p = publication
    timing = p["timing"]
    expected = {"entity":p["entity"], "component_type":p["component_type"], "stream":p["stream"], "frame":p["frame"],
                "timing":{"sample_unix_ns":str(timestamp(timing["sample_time"])), "clock_domain":timing["clock_domain"]},
                "ttl_ns":str(p["ttl_ns"]), "payload_json":p["payload"]}
    if timing["error_known"]: expected["timing"]["error_known"] = True
    for key in ("error_bound_ns", "age_hint_ns"):
        if timing[key]: expected["timing"][key] = str(timing[key])
    def provenance(value):
        out = {"origin":"ORIGIN_"+value["origin"].upper()}
        for key in ("id", "app_version", "model_id", "model_version"):
            if value.get(key): out[key] = value[key]
        if "simulation" in value:
            out["simulation"] = dict(value["simulation"])
            if out["simulation"]["seed"]: out["simulation"]["seed"] = str(out["simulation"]["seed"])
            else: del out["simulation"]["seed"]
        if value.get("inputs"): out["inputs"] = [provenance(x) for x in value["inputs"]]
        return out
    expected["provenance"] = provenance(p["provenance"])
    return expected


def proof(receipt, service, plan, states):
    raw = binary(receipt["signed_record_json"], MAX_PROOF)
    require(sha(raw) == digest(receipt["proof_sha256"]), "proof hash differs from exact bytes")
    signed = shape(strict_json(raw, MAX_PROOF), ("delegation", "record", "signature"))
    require(len(binary(signed["signature"], 64)) == 64, "record signature shape")
    record_raw = binary(signed["record"], 64 << 10)
    require(sha(record_raw) == digest(receipt["record_sha256"]), "inner record hash differs")
    r = shape(strict_json(record_raw, 64 << 10), ("version", "authority_domain", "delegation_sha256", "scope", "epoch", "sequence", "issued_at_unix_ms", "expires_at_unix_ms", "publication"))
    scope = r["scope"]
    stream = SENSORS[service] if service in SENSORS else "hazard"
    component = CAPTURE if service in SENSORS else RESULT
    expected = {"tenant":plan["tenant"], "site":plan["site"], "device":plan["device"], "app":APP, "service":service,
                "deployment_generation":states[service]["generation"], "execution_domain":"simulation", "publish":[{"component_type":component, "stream":stream}]}
    require(scope == expected and r["version"] == 1 and r["authority_domain"] == "physical", "record source differs from commissioned service")
    integer(r["epoch"], 1, (1 << 64)-1); integer(r["sequence"], 1, (1 << 64)-1)
    integer(r["issued_at_unix_ms"], 1); integer(r["expires_at_unix_ms"], r["issued_at_unix_ms"]+1)
    d = shape(signed["delegation"], ("claims", "chain_der", "signature"))
    claims_raw = binary(d["claims"], 16 << 10)
    require(sha(claims_raw) == r["delegation_sha256"], "delegation hash differs")
    claims = shape(strict_json(claims_raw, 16 << 10), ("version", "authority_domain", "scope", "epoch", "issued_at_unix_ms", "expires_at_unix_ms", "public_key"))
    require(claims["version"] == 1 and claims["authority_domain"] == "physical" and claims["scope"] == scope and claims["epoch"] == r["epoch"], "delegation source/version differs")
    require(0 < claims["issued_at_unix_ms"] <= r["issued_at_unix_ms"] < r["expires_at_unix_ms"] <= claims["expires_at_unix_ms"], "record exceeds delegation window")
    require(len(binary(claims["public_key"], 32)) == 32 and len(binary(d["signature"], 3309)) == 3309, "delegation signature/key shape")
    require(isinstance(d["chain_der"], list) and 1 <= len(d["chain_der"]) <= 8, "certificate chain bound")
    for cert in d["chain_der"]: binary(cert, 32 << 10)
    p = shape(r["publication"], ("entity", "component_type", "stream", "frame", "timing", "ttl_ns", "provenance", "payload", "tombstone"))
    require(p["component_type"] == component and p["stream"] == stream and p["frame"] == plan["frame"] and p["tombstone"] is False, "publication differs from scope/frame")
    integer(p["ttl_ns"], 1, 24*3600*10**9)
    shape(p["timing"], ("sample_time", "clock_domain", "error_known", "error_bound_ns", "age_hint_ns"))
    require(p["timing"]["clock_domain"] == "simulation:"+plan["run_id"] and p["timing"]["error_known"] is True, "source clock differs")
    for key in ("error_bound_ns", "age_hint_ns"): integer(p["timing"][key], 0, p["ttl_ns"])
    payload = strict_json(binary(p["payload"], 32 << 10), 32 << 10)
    request = strict_json(binary(receipt["request_json"], 64 << 10), 64 << 10)
    require(request == proto_publication(p), "submitted request differs from acknowledged publication")
    source = {key:scope[key] for key in ("device", "app", "service")}; source["stream"] = stream
    ref = {"key":{"tenant":scope["tenant"],"site":scope["site"],"entity":p["entity"],"type":component,"source":source},
           "version":{"epoch":r["epoch"],"sequence":r["sequence"]}, "envelope_sha256":sha(raw), "record_sha256":sha(record_raw)}
    return {"raw":raw,"record":r,"publication":p,"payload":payload,"reference":ref,"claims":claims_raw}


def capture(value, plan, scene, sensor):
    shape(value, ("version", "scene_boot_id", "captured_unix_ns", "simulation_anchor_unix_ns", "sample_json"), ("sensorlink",))
    require(value["version"] == 1 and value["scene_boot_id"] == scene["boot_id"] and value["simulation_anchor_unix_ns"] == plan["anchor_unix_ns"], "capture boot or anchor differs")
    integer(value["captured_unix_ns"], 1)
    sample = strict_json(binary(value["sample_json"], 32 << 10), 32 << 10)
    shape(sample, ("schema_version","id","site_id","frame","sensor_id","kind","sequence","sample_ns","run_id","scenario_version","seed","package_sha256","sensor_model","position_m","uncertainty_m","calibration_revision"), ("camera","audio","gas","evidence"))
    require(sample["schema_version"] == "wendy.sensor.sample.v1" and sample["sensor_id"] == sensor and sample["site_id"] == plan["site"] and sample["frame"] == plan["frame"] and sample["run_id"] == plan["run_id"] and sample["package_sha256"] == plan["package_sha256"] and sample["scenario_version"] == plan["scenario_version"] and sample["seed"] == plan["seed"], "sample commissioning differs")
    integer(sample["sequence"], 1, (1 << 64)-1); integer(sample["sample_ns"], 0, 24*3600*10**9)
    require(sample["id"] == plan["run_id"]+"/"+sensor+"/"+str(sample["sequence"]), "native sample identity differs")
    kind = "camera" if sensor.startswith("camera-") else "audio" if sensor == "audio-1" else "gas"
    require(sample["kind"] == kind and kind in sample and all(k not in sample for k in {"camera","gas","audio"}-{kind}), "sample modality differs")
    require(bool(sample["sensor_model"]) and bool(sample["calibration_revision"]), "capture model/calibration missing")
    if kind == "gas": require("evidence" not in sample, "scalar has media")
    else: evidence(sample.get("evidence"))
    return sample


def simulation(plan):
    return {"run_id":plan["run_id"],"scenario_version":plan["scenario_version"],"seed":plan["seed"],"clock_domain":"simulation:"+plan["run_id"]}


def publisher_receipt(receipt, service, plan, scene, states):
    shape(receipt, ("capture_id","native_sequence","scene_boot_id","captured_unix_ns","sample_ns","request_json","signed_record_json","record_sha256","proof_sha256"), ("evidence_sha256",))
    item = proof(receipt, service, plan, states)
    c, p = item["payload"], item["publication"]
    s = capture(c, plan, scene, SENSORS[service])
    require(p["entity"] == "sensor/"+s["sensor_id"] and timestamp(p["timing"]["sample_time"]) == plan["anchor_unix_ns"]+s["sample_ns"], "source time/entity restamped")
    require(p["provenance"] == {"origin":"simulated","app_version":"world-sensor-capture-v1","model_id":s["sensor_model"],"model_version":s["scenario_version"],"simulation":simulation(plan)}, "raw provenance rewritten")
    expected = {"capture_id":s["id"], "native_sequence":s["sequence"], "scene_boot_id":c["scene_boot_id"], "captured_unix_ns":c["captured_unix_ns"], "sample_ns":s["sample_ns"]}
    require(all(receipt[k] == v for k,v in expected.items()), "receipt differs from original capture")
    ref = c["sensorlink"]["evidence"] if "sensorlink" in c else s.get("evidence")
    require(receipt.get("evidence_sha256") == (ref["sha256"] if ref else None), "receipt evidence differs")
    require(("sensorlink" in c) == (service == "sensorlink-bridge"), "unexpected wire binding")
    item.update(sample=s, capture=c, media=ref)
    return item


def wrapper(collected):
    shape(collected, ("exit.json", "stdout.json"))
    w = shape(collected["exit.json"], ("version","termination","exit_code","stdout_bytes","stderr_bytes","output_truncated","succeeded","child_started"), ("signal","forwarded_signal"))
    require(w["version"] == 1 and w["termination"] == "completed" and w["exit_code"] == 0 and w["succeeded"] is True and w["child_started"] is True and w["output_truncated"] is False and not w.get("signal") and not w.get("forwarded_signal"), "service did not complete normally")
    integer(w["stdout_bytes"], 1, MAX_APP); integer(w["stderr_bytes"], 0, 64 << 10)
    value = collected["stdout.json"]
    bounded(value)
    require(len(json.dumps(value, separators=(",",":"), ensure_ascii=False).encode()) <= MAX_APP, "app receipt exceeds byte bound")
    return value


def validate_receipts(plan, scene, states, collected):
    """Validate the one seven-service run. Inputs must be strict decoded JSON.

    The runner checks actual stdout length against exit.json before calling.
    Plan tenant/device come from its independent normal-enrollment receipt.
    The long-running scene is checked through its nonce-bound live clock/task,
    while the other six services must have successful terminal wrapper receipts.
    """
    try:
        return _validate(plan, scene, states, collected)
    except Refused:
        raise
    except (KeyError, TypeError, ValueError, OverflowError, RecursionError) as error:
        raise Refused("malformed or inconsistent multimodal receipts") from error


def _validate(plan, scene, states, collected):
    require(plan["app"] == APP and plan["tenant"] and plan["device"] and plan["simulation_only"] is True, "commissioned simulation identity required")
    require(set(states) == set(SERVICES) and set(collected) == set(SERVICES)-{"scene"}, "all seven services required")
    require(states["scene"] == scene["task"] and not states["scene"]["world_socket"], "scene continuity state differs")
    require(all(scene[k] == plan[k] for k in ("run_id","site","frame","package_sha256","anchor_unix_ns")), "scene commissioning differs")
    sockets, pids = set(), set()
    for service, state in states.items():
        require(state["app"] == APP and state["service"] == service and state["container"] == APP+"_"+service, "container service differs")
        integer(state["pid"], 2)
        require(state["pid"] not in pids and state["cgroup"], "tasks must have distinct process identities"); pids.add(state["pid"])
        has_world = service in SENSORS or service == "fusion"
        require(state["world_socket"] is has_world, "unexpected service World authority")
        if has_world:
            integer(state["generation"], 1, (1 << 64)-1)
            path = state["socket_directory"]
            require(isinstance(path,str) and path.startswith("/") and path not in sockets, "World sockets must differ"); sockets.add(path)
    outputs = {service:wrapper(value) for service,value in collected.items()}
    by_hash, by_service = {}, {}
    for service in SENSORS:
        out = outputs[service]
        fields = ("version","run_id","sensor_id","status","receipts")
        shape(out, fields, ("duplicates",) if service != "sensorlink-bridge" else ("mode","dropped_frames"))
        require(out["version"] == 1 and out["run_id"] == plan["run_id"] and out["sensor_id"] == SENSORS[service] and out["status"] == "complete", "publisher did not complete")
        count = 32 if service == "sensorlink-bridge" else 64
        require(isinstance(out["receipts"],list) and (1 <= len(out["receipts"]) <= count if service == "sensorlink-bridge" else len(out["receipts"]) == count), "publisher capture count differs")
        if service == "sensorlink-bridge": require(out.get("mode") == "bridge" and integer(out.get("dropped_frames",0),0,31) == 32-len(out["receipts"]), "bridge drop count differs from missing finite frames")
        else: integer(out.get("duplicates",0), 0, 10000)
        rows = [publisher_receipt(x,service,plan,scene,states) for x in out["receipts"]]
        for i,item in enumerate(rows):
            h = item["reference"]["envelope_sha256"]
            require(h not in by_hash, "duplicate publication proof"); by_hash[h] = item
            if i:
                prev = rows[i-1]
                require(item["claims"] == prev["claims"] and item["record"]["sequence"] > prev["record"]["sequence"] and item["sample"]["sequence"] > prev["sample"]["sequence"] and item["sample"]["sample_ns"] > prev["sample"]["sample_ns"] and item["capture"]["captured_unix_ns"] > prev["capture"]["captured_unix_ns"], "source restarted or capture order regressed")
        by_service[service] = rows
    source = shape(outputs["sensorlink-source"], ("version","mode","run_id","sensor_id","status","frames_sent","chunks_sent","frames"), ("chunks_omitted",))
    require(source["version"] == 1 and source["mode"] == "source" and source["run_id"] == plan["run_id"] and source["sensor_id"] == "camera-b" and source["status"] == "complete" and source["frames_sent"] == 32 and len(source["frames"]) == 32 and source.get("chunks_omitted",0) == 0, "SensorLink source incomplete")
    chunks = 0
    bridge_rows = by_service["sensorlink-bridge"]
    wire_ids = [x["capture"]["sensorlink"]["frame_sequence"] for x in bridge_rows]
    require(wire_ids == sorted(set(wire_ids)) and wire_ids[-1] == 32 and all(type(x) is int and 1 <= x <= 32 for x in wire_ids), "bridge did not reach exact final wire frame")
    wire_rows = dict(zip(wire_ids,bridge_rows))
    for i,frame in enumerate(source["frames"],1):
        shape(frame,("metadata","chunks","omitted_chunks"))
        chunks += integer(frame["chunks"],1,8192)
        require(frame["omitted_chunks"] == 0,"SensorLink chunk omitted")
        m = shape(frame["metadata"], ("version","channel_id","frame_sequence","timestamp_us","flags","jpeg_sha256","jpeg_size","capture_json"))
        original = strict_json(binary(m["capture_json"],32 << 10),32 << 10)
        capture(original,plan,scene,"camera-b")
        require(m["version"] == 1 and m["frame_sequence"] == i and "sensorlink" not in original, "source wire frame identity differs")
        item = wire_rows.get(i)
        if item is None: continue
        c, wire = item["capture"], item["capture"]["sensorlink"]
        require({k:v for k,v in c.items() if k != "sensorlink"} == original and "sensorlink" not in original, "bridge changed original capture bytes")
        shape(wire,("protocol","channel_id","frame_sequence","timestamp_us","flags","scene_boot_id","capture_sequence","captured_unix_ns","sample_sha256","calibration_sha256","evidence"))
        require(m["version"] == 1 and m["frame_sequence"] == i and wire["frame_sequence"] == i and wire["protocol"] == "wendylite.v1.sensorlink" and wire["scene_boot_id"] == c["scene_boot_id"] and wire["capture_sequence"] == item["sample"]["sequence"] and wire["captured_unix_ns"] == c["captured_unix_ns"] and wire["sample_sha256"] == sha(binary(c["sample_json"],32 << 10)), "wire capture identity differs")
        digest(wire["calibration_sha256"])
        require(all(m[k] == wire[k] for k in ("channel_id","timestamp_us","flags")) and wire["timestamp_us"] == c["captured_unix_ns"]//1000, "wire timestamp/channel differs")
        require(evidence(wire["evidence"]) == {"sha256":m["jpeg_sha256"],"size":m["jpeg_size"],"content_type":"image/jpeg"} and item["sample"]["evidence"]["content_type"] == "image/png", "JPEG delivery or original PNG link differs")
    require(source["chunks_sent"] == chunks, "SensorLink chunk totals differ")
    return validate_fusion(outputs["fusion"],by_hash,plan,scene,states,by_service,chunks)


def validate_fusion(out, by_hash, plan, scene, states, by_service, chunks):
    shape(out,("version","run_id","status","attempts","evaluation","inputs","media","request_json","signed_record_json","record_sha256","proof_sha256","locally_verified"))
    require(out["version"] == 1 and out["run_id"] == plan["run_id"] and out["status"] == "complete" and out["locally_verified"] is True,"fusion did not report verified completion")
    integer(out["attempts"],1,16)
    require(len(out["inputs"]) == 4 and len(out["media"]) == 3,"fusion input/media count differs")
    used = {}
    for value in out["inputs"]:
        shape(value,("reference","envelope_json"))
        raw = binary(value["envelope_json"],MAX_PROOF)
        h = sha(raw); item = by_hash.get(h)
        require(item is not None and raw == item["raw"] and value["reference"] == item["reference"],"fusion proof was not an exact publisher acknowledgment")
        service = item["record"]["scope"]["service"]
        require(service not in used,"duplicate fusion source"); used[service] = item
    require(set(used) == set(SENSORS),"fusion missing a required source")
    delivered, total = set(), 0
    for value in out["media"]:
        shape(value,("source","reference","data"))
        owner = value["source"]; service = owner.get("service"); item = used.get(service)
        require(item is not None and item["media"] is not None and service not in delivered,"unexpected or duplicate media owner")
        expected = {"app":APP,"service":service,"component_type":CAPTURE,"stream":SENSORS[service]}
        require(owner == expected and evidence(value["reference"]) == item["media"],"media selector/reference differs")
        data = binary(value["data"],8 << 20); total += len(data)
        require(total <= 16 << 20 and len(data) == item["media"]["size"] and sha(data) == item["media"]["sha256"],"media bytes differ")
        mime = item["media"]["content_type"]
        require((mime == "image/png" and data.startswith(b"\x89PNG\r\n\x1a\n")) or (mime == "image/jpeg" and data.startswith(b"\xff\xd8") and data.endswith(b"\xff\xd9")) or (mime == "audio/wav" and data.startswith(b"RIFF") and data[8:12] == b"WAVE"),"media format marker differs")
        delivered.add(service)
    require(delivered == {"camera-a","audio","sensorlink-bridge"},"required media absent")
    ack = proof(out,"fusion",plan,states)
    result = shape(ack["payload"],("version","result_schema","result_json","inputs","retain_until_unix_ms"))
    require(result["version"] == 1 and result["result_schema"] == "wendy.hazard.v1" and len(result["inputs"]) == 4,"derived envelope differs")
    integer(result["retain_until_unix_ms"],ack["record"]["issued_at_unix_ms"]+1)
    expected_refs = {x["reference"]["envelope_sha256"]:x["reference"] for x in used.values()}
    require({r["envelope_sha256"]:r for r in result["inputs"]} == expected_refs,"derived exact input references differ")
    hazard = strict_json(binary(result["result_json"],24 << 10),24 << 10)
    evaluation = shape(out["evaluation"],("state","reason","input_digest","hazard"))
    require(evaluation["state"] == "hazard" and evaluation["hazard"] == hazard and evaluation["input_digest"] == hazard["input_digest"],"derived hazard differs from evaluation")
    digest(hazard["input_digest"])
    require(len(hazard["inputs"]) == 4,"hazard lineage count differs")
    seen, starts, deadlines = set(), [], []
    for h in hazard["inputs"]:
        service = h["source"]["service"]; item = used.get(service)
        require(item is not None and service not in seen,"hazard source missing/duplicated"); seen.add(service)
        s = copy.deepcopy(item["sample"])
        original = None
        if service == "sensorlink-bridge": original = s["evidence"]; s["evidence"] = item["media"]
        origin = {"proof_sha256":item["reference"]["envelope_sha256"],"scene_boot_id":scene["boot_id"],"simulation_anchor":h["capture"]["simulation_anchor"]}
        if original is not None: origin["original_evidence"] = original
        require(h["sample"] == s and h["sample_id"] == s["id"] and h["sensor_id"] == s["sensor_id"] and h["kind"] == s["kind"] and h["source"] == item["reference"]["key"]["source"] and h["version"] == item["reference"]["version"] and h["capture"] == origin and timestamp(origin["simulation_anchor"]) == plan["anchor_unix_ns"] and h.get("evidence") == item["media"] and "replay" not in h,"hazard changed original input lineage")
        p = item["publication"]; sample_ns = timestamp(p["timing"]["sample_time"]); span = s.get("audio",{}).get("duration_ns",0)
        margin = max(p["timing"]["error_bound_ns"],p["timing"]["age_hint_ns"])
        cap = sample_ns+p["ttl_ns"]-margin-span
        if "camera" in s: cap = min(cap,sample_ns+s["camera"]["calibration_valid_for_ns"]-margin)
        require(timestamp(h["sample_time"]) == sample_ns and timestamp(h["capture_start"]) == sample_ns-span and timestamp(h["valid_until"]) <= cap,"hazard input time/deadline differs")
        starts.append(sample_ns-span); deadlines.append(timestamp(h["valid_until"]))
    p = ack["publication"]
    require(p["entity"] == plan["run_id"]+"/possible-gas-hazard" and timestamp(p["timing"]["sample_time"]) == min(starts) and timestamp(hazard["valid_until"]) == min(deadlines) and p["ttl_ns"] == min(deadlines)-min(starts),"derived lifetime exceeds inputs")
    leaves = [{"id":"sha256:"+ref["envelope_sha256"],"origin":"simulated","simulation":simulation(plan)} for ref in result["inputs"]]
    require(p["provenance"] == {"origin":"inferred","app_version":"world-fusion-v1","model_id":"synthetic-gas-warning-rule","model_version":"1","inputs":leaves},"derived raw-leaf provenance differs")
    require(sorted(hazard["evidence"],key=lambda x:x["sha256"]) == sorted([x["media"] for x in used.values() if x["media"]],key=lambda x:x["sha256"]),"hazard evidence list differs")
    return {"version":1,"simulation_only":True,"services_checked":7,"completed_apps":6,"publisher_receipts":{k:len(v) for k,v in by_service.items()},"sensorlink_frames":32,"sensorlink_dropped_frames":32-len(by_service["sensorlink-bridge"]),"sensorlink_chunks":chunks,"fusion_input_proofs":4,"fusion_media":3,"fusion_media_bytes":total,"result_proof_sha256":out["proof_sha256"],"result_record_sha256":out["record_sha256"],"checks":"saved byte hashes, exact source/version/capture lineage and wrapper completion","cryptographic_verification":"reported by the ordinary fusion app; not independently repeated by this validator","retained_proof_retrieval_verified":False,"current_authority_verified":False}
