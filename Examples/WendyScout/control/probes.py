"""The only authenticated Wendy operations: local self, fixed commands, owned scratch app."""

import json, os, pathlib, re, time
import hashlib
from common import APP, STATE, command, save, scrub, public_observation, CANCEL
from functional import experiment, WORKFLOWS

SCRATCH = APP + ".scratch"
RECIPES = {
    "info": ["device", "info"],
    "cameras": ["device", "camera", "list"],
    "audio": ["device", "audio", "list"],
    "apps": ["device", "apps", "list"],
    "help_root": ["--help"],
    "help_run": ["run", "--help"],
    "help_device": ["device", "--help"],
    "scratch_start": ["device", "apps", "start", SCRATCH],
    "scratch_stop": ["device", "apps", "stop", SCRATCH],
    "scratch_stop_extra": ["device", "apps", "stop", SCRATCH, "scout-invalid-extra"],
    "scratch_logs": ["device", "logs", SCRATCH, "--tail", "20", "--no-follow"],
    "scratch_remove": ["device", "apps", "remove", SCRATCH, "--force"],
}


class Probes:
    def __init__(self, versions, scratch=SCRATCH, local=None):
        self.local = local
        self.scratch = scratch
        self.versions = versions
        self.socket = os.environ.get("WENDY_AGENT_SOCKET", "")
        if (
            not self.socket.startswith("/run/wendy/")
            or not pathlib.Path(self.socket).is_socket()
        ):
            raise RuntimeError(
                "Documented admin socket missing; no network/cloud fallback"
            )
        self.agent = None
        self.deadline = None

    def run(self, kind, version, run):
        if CANCEL.is_set() or (
            self.deadline is not None and time.monotonic() >= self.deadline
        ):
            raise TimeoutError("Probe batch deadline or shutdown")
        if self.local:
            self.local.verify_development()
        if kind in WORKFLOWS:
            return experiment(self, kind, version, run)
        if kind not in RECIPES or version not in self.versions:
            raise ValueError("Unapproved probe or version")
        argv = ["--json", *[self.scratch if x == SCRATCH else x for x in RECIPES[kind]]]
        env = {
            "PATH": "/usr/bin:/bin",
            "WENDY_AGENT_SOCKET": self.socket,
            "WENDY_CONFIG_DIR": str(STATE / "config" / self.versions[version]["tag"]),
            "WENDY_ANALYTICS": "false",
            "WENDY_NO_UPDATE_CHECK": "1",
        }
        started = time.monotonic()
        try:
            raw = command(
                self.versions[version]["binary"],
                argv,
                env=env,
                timeout=(
                    min(25, max(0.1, self.deadline - time.monotonic()))
                    if self.deadline
                    else 25
                ),
                detach_marker=(
                    b"SCOUT_SCRATCH_READY" if kind == "scratch_start" else None
                ),
            )
        except TimeoutError:
            raw = {"exit": 124, "output": '{"error":{"code":"scout_timeout"}}'}
        try:
            parsed = json.loads(raw["output"])
            json_valid = True
        except (json.JSONDecodeError, TypeError):
            parsed = raw["output"]
            json_valid = False
        output = public_observation(kind, parsed, raw["exit"], self.scratch)
        result = {
            "evidence_kind": "fresh_command",
            "kind": kind,
            "version": self.versions[version]["tag"],
            "command": [
                "WENDY_AGENT_SOCKET=<local-admin-socket>",
                f"wendy-{self.versions[version]['tag']}",
                *["<Scout-owned scratch ID>" if x == self.scratch else x for x in argv],
            ],
            "exit": raw["exit"],
            "json_valid": json_valid,
            "failure_identity": hashlib.sha256(
                json.dumps(
                    {
                        "exit": raw["exit"],
                        "error": scrub(parsed) if raw["exit"] else output,
                    },
                    sort_keys=True,
                ).encode()
            ).hexdigest(),
            "seconds": time.monotonic() - started,
            "output": output,
        }
        if (
            kind == "info"
            and raw["exit"] == 0
            and (version == "stable" or self.agent is None)
        ):
            info = json.loads(raw["output"])
            agent = info.get("version") if isinstance(info, dict) else None
            if not isinstance(agent, str) or not re.fullmatch(
                r"\d{4}\.\d{2}\.\d{2}-\d{6}", agent
            ):
                raise RuntimeError("Trusted Agent identity unavailable")
            if self.agent and agent != self.agent:
                raise RuntimeError("Agent changed; testing held")
            self.agent = agent
            result["agent"] = agent
            expected = os.environ.get("SCOUT_EXPECT_AGENT_VERSION", "")
            if expected and agent != expected:
                raise RuntimeError("Agent does not match operator pin")
        if run:
            number = len(list(run.glob("probe-*.json")))
            result["id"] = f"probe-{number:03d}"
            save(run / (result["id"] + ".private.json"), {"raw": raw})
            save(run / (result["id"] + ".json"), result)
        return result

    def replay(self, probe, baseline, nightly, run, predicate="functional", field=""):
        if probe not in WORKFLOWS or predicate != "functional" or field:
            raise ValueError("Only implemented functional workflows may be replayed")
        if baseline not in {"past", "stable"} or nightly != "nightly":
            raise ValueError("Expected a stable/nightly comparison")
        results = [
            self.run(probe, v, run) for v in [baseline, nightly, baseline, nightly]
        ]
        b, n = results[::2], results[1::2]
        good = all(
            r.get("completed")
            and r.get("cleanup_verified")
            and r["outcome"] == "passed"
            for r in b
        )
        bad = all(
            r.get("completed")
            and r.get("cleanup_verified")
            and r["outcome"] == "failed"
            for r in n
        )
        bad = bad and n[0].get("failure_identity") == n[1].get("failure_identity")
        chronological = (
            self.versions[baseline]["published_at"]
            <= self.versions[nightly]["published_at"]
        )
        current = (
            [self.run(probe, "stable", run), self.run(probe, "stable", run)]
            if good and bad and chronological
            else []
        )
        states = [
            (
                r["outcome"]
                if r.get("completed") and r.get("cleanup_verified")
                else "blocked"
            )
            for r in current
        ]
        newer = (
            self.versions["stable"]["published_at"]
            > self.versions[nightly]["published_at"]
        )
        fixed = bool(newer and states == ["passed", "passed"])
        existing = bool(not newer and states == ["failed", "failed"])
        uncertain = bool(
            current and states not in (["passed", "passed"], ["failed", "failed"])
        )
        return {
            "verified": bool(
                good
                and bad
                and chronological
                and not fixed
                and not existing
                and not uncertain
            ),
            "kind": "functional-regression",
            "probe": probe,
            "baseline": baseline,
            "field": "",
            "agent": self.agent,
            "already_fixed_in_newer_stable": fixed,
            "existing_stable_failure": existing,
            "current_stable_unverified": uncertain,
            "current_stable": current,
            "results": results,
            "reason": "State-equivalent functional replay; passing stable and failing nightly required",
        }
