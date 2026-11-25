"""Explicit two-phase history extension for an already owned Runner.

Nothing runs on import or construction. The parent must opt in before initial
commissioning, apply commission()/configurations(), validate the normal seven
service result, then call execute with its exact retained fusion outputs.
Runner.execute/finish remains the sole owner of runtime removal. This extension
never creates a VM/container/network, reads a signing key, extends existing media
retention, or redeploys a baseline service. Its tests use mocked Runner calls;
they do not establish a successful runtime history scenario.

Success output from livefusion omits diagnostics. An error/unknown outcome is
refused unchanged; no receipt field is stripped to make it acceptable. Original
media uploads already receive the evidence store's protected default retention
(seven days in this fixture) on first allocation. The opt-in 600000ms result
request pins proofs within existing media deadlines; it cannot extend those
deadlines. The reader checks each original deadline using its current app clock.
"""
import copy
import json
from pathlib import Path
import time

import configs
import history as h
import history_receipts as receipts
import history_stage as bundle_stage
import receipts as r
import stage
import validation

REMOTE = "/var/lib/agentmultimodal"
VOLUMES = "/var/lib/wendy/volumes/"
MAX_STDIN = 1 << 20
PHASE_DURATION_MS = 45000
TOTAL_SECONDS = 300
OUTPUTS = (("exit.json", 4096), ("stdout.json", 32 << 20), ("stderr.log", 64 << 10))


def _target(phase, generation):
    return dict(phase=phase, app=stage.APP, service=h.READER, expected_generation=generation,
                duration_ms=PHASE_DURATION_MS, request_timeout_ms=1000, poll_interval_ms=500, clock_max_width_ms=5000)


def _fusion_preflight(commission_bytes, stdout, exit_bytes):
    """Validate success and compute a worst-case config size, with no reader ID."""
    c = h.commission(commission_bytes)
    out = r.strict_json(stdout, h.MAX_INPUT)
    wrapper = r.strict_json(exit_bytes, 4096)
    r.wrapper({"exit.json": wrapper, "stdout.json": out})
    r.integer(wrapper["version"], 1, 1)
    r.integer(wrapper["exit_code"], 0, 0)
    r.require(wrapper["stdout_bytes"] == len(stdout), "fusion wrapper count differs")
    r.shape(out, ("version", "run_id", "status", "attempts", "evaluation", "inputs", "media", "request_json",
                  "signed_record_json", "record_sha256", "proof_sha256", "locally_verified"))
    r.integer(out["version"], 1, 1)
    r.require(out["status"] == "complete" and out["locally_verified"] is True and out["run_id"] == c["run_id"],
              "history needs exact successful fusion acknowledgment")
    r.require(isinstance(out["inputs"], list) and len(out["inputs"]) == 4, "four original capture proofs required")
    sources = {}
    for entry in out["inputs"]:
        r.shape(entry, ("reference", "envelope_json"))
        h.reference_shape(entry["reference"])
        service = entry["reference"]["key"]["source"]["service"]
        r.require(service in stage.SENSORS and service not in sources, "exact distinct capture sources required")
        item = h.decode_proof(r.binary(entry["envelope_json"], r.MAX_PROOF), service, c)
        r.require(item["reference"] == entry["reference"], "input reference differs from original")
        h.capture(item, service, c)
        sources[service] = item
    ack = h.decode_proof(r.binary(out["signed_record_json"], r.MAX_PROOF), "fusion", c)
    r.shape(ack["payload"], ("version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"))
    for ref in ack["payload"]["inputs"]:
        h.reference_shape(ref)
    h.hazard_shapes(out)
    r.validate_fusion(out, {r.sha(x["raw"]): x for x in sources.values()}, c,
                      {"boot_id": c["scene_boot_id"]},
                      {key: dict(generation=value) for key, value in c["publisher_generations"].items()},
                      {key: [value] for key, value in sources.items()}, 0)
    sources["fusion"] = ack
    expected = [dict(reference=sources[s]["reference"], envelope_json=out["signed_record_json"] if s == "fusion"
                     else next(x["envelope_json"] for x in out["inputs"] if x["reference"] == sources[s]["reference"]))
                for s in h.ORDER]
    r.require(sum(len(s["raw"]) for s in sources.values()) <= h.MAX_TOTAL_PROOFS, "original proof budget exceeded")
    # Only a size calculation. This maximum integer is never a commissioned
    # identity, emitted Prepared artifact, deployed config or clock pin.
    maximum = {"version": 1, **{key: c[key] for key in ("tenant", "site", "device", "app", "run_id")},
               **{key: value for key, value in _target(h.PHASES[0], (1 << 64)-1).items() if key not in ("phase", "app")},
               "expected": expected}
    r.require(len(h.encode(maximum)) <= MAX_STDIN, "reader config exceeds existing Runner stdin bound")
    return c


# Every receiver is finite, writes only the explicitly selected new path, and
# fails rather than retrying an uncertain write. No secret configuration is read.
WRITE_CHUNK = r'''# history:write-chunk
import hashlib,os,pathlib,stat,sys
path,offset,total,digest,mode=sys.argv[1:];offset=int(offset);total=int(total);mode=int(mode)
data=sys.stdin.buffer.read((1<<20)+1);assert 0<len(data)<=1<<20 and 0<=offset<total<=64<<20 and offset+len(data)<=total
p=pathlib.PurePosixPath(path);assert p.is_absolute() and str(p)==path and '..' not in p.parts
fd=os.open('/',os.O_RDONLY|os.O_DIRECTORY)
try:
 for part in p.parts[1:-1]:
  child=os.open(part,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd);os.close(fd);fd=child
  st=os.fstat(fd);assert st.st_uid==0 and not st.st_mode&0o022
 flags=os.O_RDWR|os.O_NOFOLLOW|(os.O_CREAT|os.O_EXCL if offset==0 else 0)
 out=os.open(p.name,flags,mode,dir_fd=fd)
 try:
  st=os.fstat(out);assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and st.st_size==offset and not st.st_mode&0o022
  os.lseek(out,offset,os.SEEK_SET)
  with os.fdopen(os.dup(out),'wb') as f:f.write(data);f.flush()
  if offset+len(data)==total:
   os.lseek(out,0,os.SEEK_SET)
   with os.fdopen(os.dup(out),'rb') as f:assert hashlib.file_digest(f,'sha256').hexdigest()==digest
   os.fchmod(out,mode);os.fsync(out);os.fsync(fd)
 finally:os.close(out)
finally:os.close(fd)
print('exact chunk accepted')
'''

INSTALL_MANIFEST = r'''# history:install-manifest
import hashlib,os,stat,sys
directory,previous,wanted,phase=sys.argv[1:];data=sys.stdin.buffer.read((1<<20)+1)
assert 0<len(data)<=1<<20 and hashlib.sha256(data).hexdigest()==wanted and phase in ('before_restart','after_restart')
fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
try:
 st=os.fstat(fd);assert st.st_uid==0 and not st.st_mode&0o022
 old=os.open('wendy.json',os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
 with os.fdopen(old,'rb') as f:
  st=os.fstat(f.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==0 and not st.st_mode&0o022
  current=f.read((1<<20)+1);assert len(current)<=1<<20 and hashlib.sha256(current).hexdigest()==previous
 name='.history-'+phase+'.json';new=os.open(name,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o644,dir_fd=fd)
 with os.fdopen(new,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())
 os.replace(name,'wendy.json',src_dir_fd=fd,dst_dir_fd=fd);os.fsync(fd)
finally:os.close(fd)
print('fenced history manifest installed')
'''


class HistoryRuntime:
    """Opt-in hooks; only execute performs runtime operations.

    Runner's task()/deploy()/release()/snapshot_outputs() remain unchanged and
    accept only the original seven services. This helper handles one fixed
    read-only service without widening their selection or entitlements.
    """
    def __init__(self, runner, bundle, *, reader_sha256, baseline_plan_sha256, baseline_inventory_sha256):
        self.runner = runner
        self.bundle = Path(bundle)
        self.pins = dict(reader_sha256=reader_sha256, baseline_plan_sha256=baseline_plan_sha256,
                         baseline_inventory_sha256=baseline_inventory_sha256)
        self.plan = bundle_stage.validate_bundle(self.bundle, runner.source, **self.pins)
        r.require(self.plan["run_id"] == runner.plan["run_id"], "history and runtime run differ")
        self._commissioned = self._configured = self._used = False
        self.events, self.attempted = [], []
        self.deadline = None
        self.directory = runner.out / "history"

    def commission(self, spec):
        r.require(not self._used and not self._commissioned and spec == configs.commission_spec(self.runner.scene),
                  "exact once-only initial scene commissioning hook required")
        self._commissioned = True
        return {**copy.deepcopy(spec), "history_reader": True}

    def configurations(self, values):
        r.require(not self._used and not self._configured and set(values) == set(stage.SERVICES)-{"scene"},
                  "exact once-only baseline configuration hook required")
        r.require(values["fusion"]["retain_for_ms"] == 60000, "unexpected baseline result retention")
        result = copy.deepcopy(values)
        result["fusion"]["retain_for_ms"] = 600000
        self._configured = True
        return result

    def source_commission(self):
        r = self.runner
        return h.encode(dict(version=1, tenant=r.public["tenant"], device=r.public["principal"],
                             **{key: r.plan[key] for key in ("site", "app", "run_id", "frame", "package_sha256",
                                                            "anchor_unix_ns", "scenario_version", "seed")},
                             scene_boot_id=r.scene["boot_id"],
                             publisher_generations={s: r.states[s]["generation"] for s in h.ORDER}))

    def _budget(self, maximum=30):
        remaining = self.deadline-time.monotonic()
        r.require(remaining > 0, "history orchestration deadline expired")
        return min(maximum, remaining)

    def _event(self, phase, stage_name, **fields):
        r.require(len(self.events) < 64, "history phase log bound exceeded")
        self.events.append(dict(phase=phase, stage=stage_name, **fields))

    def _python(self, script, *args, check=True, timeout=30):
        return self.runner.python(script, *args, check=check, timeout=self._budget(timeout))

    def _stdin(self, script, args, data):
        r.require(isinstance(data, bytes) and 0 < len(data) <= MAX_STDIN, "history stdin bound exceeded")
        return self.runner.call(["docker", "exec", "-i", self.runner.name, "python3", "-c", script, *map(str, args)],
                                timeout=self._budget(15), input_data=data)

    def _scene(self, previous):
        self._budget()
        observed = self.runner.observe_scene()
        self._budget()
        validation.scene_unchanged(previous, observed)
        return observed

    def _upload(self, files):
        self._python("# history:new-context\nimport os; os.mkdir('/var/lib/agentmultimodal/project/services/proof-reader',0o755)")
        for name, data, mode in files:
            digest = r.sha(data)
            for offset in range(0, len(data), MAX_STDIN):
                self._stdin(WRITE_CHUNK, (REMOTE+"/"+bundle_stage.CONTEXT+"/"+name, offset, len(data), digest, mode),
                            data[offset:offset+MAX_STDIN])

    def _task(self):
        """Observe the exact new reader task; never infer identity from its IP."""
        cid = stage.APP+"_"+h.READER
        info = stage.strict_json(self.runner.inside("ctr", "containers", "info", cid, timeout=self._budget(10)).stdout)
        labels = info.get("Labels", {})
        r.require(labels.get("sh.wendy/app.id") == stage.APP and labels.get("sh.wendy/service") == h.READER,
                  "unexpected history task identity")
        rows = self.runner.inside("ctr", "tasks", "list", timeout=self._budget(10)).stdout.decode().splitlines()[1:]
        matches = [row.split() for row in rows if row.split() and row.split()[0] == cid]
        r.require(len(matches) == 1 and len(matches[0]) >= 3 and matches[0][2] == "RUNNING", "reader gate task is not running")
        pid = int(matches[0][1]); r.integer(pid, 2)
        state = stage.strict_json(self._python("# history:reader-task\nimport json,os,sys; p=int(sys.argv[1]); raw=open('/proc/'+str(p)+'/stat').read(); fields=raw.rsplit(')',1)[1].split(); print(json.dumps({'pid':p,'start_ticks':int(fields[19]),'netns_inode':os.stat('/proc/'+str(p)+'/ns/net').st_ino,'cgroup':open('/proc/'+str(p)+'/cgroup').read().strip()}))", pid).stdout)
        interfaces = stage.strict_json(self.runner.inside("nsenter", "--target", str(pid), "--net", "ip", "-j", "-4", "addr", "show", "dev", "eth0", timeout=self._budget(10)).stdout)
        ips = [a["local"] for i in interfaces for a in i.get("addr_info", []) if a.get("family") == "inet"]
        r.require(len(ips) == 1, "one reader CNI IPv4 required")
        state.update(container=cid, app=stage.APP, service=h.READER, ipv4=ips[0],
                     generation=int(labels.get("sh.wendy/world.deployment-generation", "0")),
                     socket_directory=labels.get("sh.wendy/world.socket-directory", ""),
                     world_socket=bool(labels.get("sh.wendy/world.socket-directory")))
        receipts._task(state, h.READER, True)
        r.require(state["pid"] == pid, "observed reader PID differs")
        return state

    def _snapshot(self, phase, name):
        destination = self.directory / phase["name"] / name
        source = VOLUMES+phase["volumes"]["proofs"]
        result = {}
        for filename, limit in OUTPUTS:
            try:
                result[filename] = self.runner.snapshot_file(source+"/"+filename, destination/filename, limit)
            except Exception as exc:
                result[filename] = dict(status="error", error_type=type(exc).__name__)
        stage.write_new(destination/"snapshot.json", stage.json_bytes(result))
        return destination, result

    def _phase(self, phase, manifest, commission, stdout, exit_bytes, previous, prior_task, scene):
        name = phase["name"]
        self.attempted.append(phase)
        self._event(name, "start")
        r.require(self.runner.resource_state("container", self.runner.name) == "owned", "history runtime ownership changed")
        self._budget()
        self._python("# history:new-volumes\nimport json,os,sys; names=json.loads(sys.argv[1]); base='/var/lib/wendy/volumes'; assert set(names)=={'config','proofs'}\nfor name in names.values():\n assert '/' not in name; os.mkdir(base+'/'+name,0o700)\nprint('fresh history phase volumes')", json.dumps(phase["volumes"]))
        self._stdin(INSTALL_MANIFEST, (REMOTE+"/project", phase["expected_previous_manifest_sha256"], phase["manifest_sha256"], name), manifest)
        self._event(name, "manifest_installed")
        # The exact fixed service is outside the default seven-service selector.
        # No --force, --all, start or restart command touches baseline services.
        self.runner.cli("run", "--prefix", REMOTE+"/project", "--builder", "buildkit", "--chunking", "force",
                        "--detach", "--no-restart", "--skip-cloud-registration", "--yes", "--service", h.READER,
                        timeout=self._budget(90))
        self._budget()
        task = self._task()
        scene = self._scene(scene)
        if prior_task is not None:
            r.require(task["generation"] != prior_task["generation"] and task["socket_directory"] != prior_task["socket_directory"],
                      "reader redeployment reused old authority or socket")
        r.require(task["socket_directory"] not in {s.get("socket_directory") for s in self.runner.states.values()},
                  "reader reused a baseline service socket")
        prepared = h.prepare(commission, stdout, exit_bytes, h.encode(_target(name, task["generation"])), previous)
        config, _ = h.validate_prepared(prepared)
        receipts._observations(config, task, scene)
        r.require(len(prepared.config) <= MAX_STDIN, "actual reader config exceeds Runner stdin bound")
        h.write_phase(self.directory, prepared)
        stage.write_new(self.directory/name/"task.json", stage.json_bytes(task))
        stage.write_new(self.directory/name/"scene.json", stage.json_bytes(scene))
        transfer = h.stdin_transfer(prepared, VOLUMES+phase["volumes"]["config"]+"/config.json")
        self.runner.call(["docker", "exec", "-i", self.runner.name, *transfer.argv], timeout=self._budget(15), input_data=transfer.stdin)
        self._event(name, "config_installed", generation=task["generation"], config_sha256=r.sha(prepared.config))
        self._python("# history:release-gate\nimport hashlib,os,pathlib,sys; p=pathlib.Path(sys.argv[1]); assert hashlib.sha256((p/'config.json').read_bytes()).hexdigest()==sys.argv[2]; fd=os.open(p/'start',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600); os.close(fd); print('reader gate released')", VOLUMES+phase["volumes"]["config"], r.sha(prepared.config))
        self._event(name, "gate_released")
        limit = min(self.deadline, time.monotonic()+PHASE_DURATION_MS/1000+15)
        wait_error = None
        try:
            while True:
                r.require(time.monotonic() < limit, "reader completion deadline expired")
                done = self._python("# history:reader-done\nimport os,sys; assert os.path.isfile(sys.argv[1])", VOLUMES+phase["volumes"]["proofs"]+"/exit.json", check=False, timeout=5)
                if done.returncode == 0:
                    break
                time.sleep(min(.25, max(0, limit-time.monotonic())))
        except BaseException as exc:
            wait_error = exc
        destination, captured = self._snapshot(phase, "receipts")
        if wait_error is not None:
            raise wait_error
        r.require(all(v.get("status") == "captured" for v in captured.values()), "incomplete original history outputs")
        summary = receipts.validate_history_receipts(prepared, (destination/"exit.json").read_bytes(),
                    (destination/"stdout.json").read_bytes(), (destination/"stderr.log").read_bytes(), task, scene)
        scene = self._scene(scene)
        stage.write_new(self.directory/name/"validated.json", stage.json_bytes(summary))
        self._event(name, "validated", proof_bundle_sha256=summary["proof_bundle_sha256"])
        return prepared, task, scene, summary

    def execute(self, commission_bytes, fusion_stdout_bytes, fusion_exit_bytes):
        """Execute once after success; all failure output collection precedes cleanup.

        The parent must call this inside Runner.run so its existing finally path
        still snapshots stopped stores and removes only owned resources. Do not
        call Runner.finish here: it would destroy the baseline on success too.
        """
        r.require(not self._used and self._commissioned and self._configured, "history hooks missing or execution reused")
        self._used = True
        c = _fusion_preflight(commission_bytes, fusion_stdout_bytes, fusion_exit_bytes)
        r.require(h.encode(c) == self.source_commission(), "historical source commissioning differs from independent runtime")
        # Revalidate everything locally and own all upload bytes before mutation.
        r.require(bundle_stage.validate_bundle(self.bundle, self.runner.source, **self.pins) == self.plan, "history bundle changed")
        files = []
        for name in ("Dockerfile", "app", "app-runner"):
            relative = bundle_stage.CONTEXT+"/"+name
            limit, mode = bundle_stage.FILES[relative]
            data = bundle_stage._bytes(self.bundle/relative, limit)
            expected = self.plan["binaries"].get(h.READER if name == "app" else name)
            if expected is not None:
                r.require(dict(sha256=r.sha(data), size=len(data)) == expected, "history binary changed after validation")
            else:
                r.require(data == stage.dockerfile(h.READER, self.plan["run_id"], self.runner.plan["anchor_unix_ns"],
                                                   self.runner.plan["seed"], {}), "history launcher changed after validation")
            files.append((name, data, mode))
        manifests = [bundle_stage._bytes(self.bundle/p["manifest"], MAX_STDIN) for p in self.plan["phases"]]
        r.require(all(r.sha(raw) == p["manifest_sha256"] for p, raw in zip(self.plan["phases"], manifests)), "history manifest changed")
        self.directory.mkdir(mode=0o700)
        stage.write_new(self.directory/"source-commission.json", commission_bytes)
        self.deadline = time.monotonic()+TOTAL_SECONDS
        result = dict(version=1, simulation_only=True, passed=False, phases=[], runtime_cleanup_owner="Runner.execute/finish")
        try:
            r.require(self.runner.resource_state("container", self.runner.name) == "owned", "history runtime is not owned")
            scene = self._scene(self.runner.scene)
            self._upload(files)
            previous = task = None
            for index, (phase, manifest) in enumerate(zip(self.plan["phases"], manifests)):
                if index:
                    self._event(phase["name"], "agent_restart_started", previous_pid=self.runner.agent_pid)
                    old_pid = self.runner.agent_pid
                    self._budget(); self.runner.stop_agent(); self._budget()
                    self.runner.start_agent("history-restarted"); self._budget()
                    r.require(self.runner.agent_pid != old_pid, "agent restart kept old process identity")
                    scene = self._scene(scene)
                    self._event(phase["name"], "agent_restarted", pid=self.runner.agent_pid)
                previous, task, scene, summary = self._phase(phase, manifest, commission_bytes, fusion_stdout_bytes,
                                                           fusion_exit_bytes, previous, task, scene)
                if result["phases"]:
                    original = result["phases"][0]
                    r.require(summary["proofs"] == original["proofs"] and summary["media"] == original["media"],
                              "original historical bytes or retention changed across restart")
                result["phases"].append(summary)
            self._budget()
            result.update(passed=True, same_originals_across_restart=True, scene_preserved=True,
                          current_source_authority=False, qualification="Two reader-reported historical retrievals with exact saved-byte checks; no physical operation or new source authority.")
            return result
        except BaseException as exc:
            result["error_type"] = type(exc).__name__
            raise
        finally:
            # Even CLI/config/gate/clock/receipt failures retain every available
            # original output before the parent's global cleanup. No validation
            # error changes a failed phase into success or permits a retry.
            for phase in self.attempted:
                try:
                    self._snapshot(phase, "final-outputs")
                except Exception as exc:
                    result.setdefault("collection_errors", []).append(dict(phase=phase["name"], error_type=type(exc).__name__))
            stage.write_new(self.directory/"phase-log.json", stage.json_bytes(dict(version=1, events=self.events)))
            stage.write_new(self.directory/"result.json", stage.json_bytes(result))
