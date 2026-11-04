"""The only authenticated Wendy operations: local self, fixed commands, owned scratch app."""

import json, os, pathlib, re, time
import hashlib
from common import APP, STATE, command, save, scrub, public_observation, CANCEL

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
        if kind == "scratch_lifecycle":
            return self.lifecycle(version, run)
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
        raw = command(
            self.versions[version]["binary"],
            argv,
            env=env,
            timeout=min(25, max(0.1, self.deadline - time.monotonic()))
            if self.deadline
            else 25,
            detach_marker=b"SCOUT_SCRATCH_READY" if kind == "scratch_start" else None,
        )
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

    def lifecycle(self, version, run):
        if version not in self.versions or not self.local:
            raise ValueError("Unapproved lifecycle")
        self.local.cleanup_scratch()
        self.local.create_scratch()
        rows = []
        try:
            for action, expected in [
                ("scratch_start", "RUNNING"),
                ("scratch_stop", "STOPPED"),
                ("scratch_remove", "ABSENT"),
            ]:
                rows.append(self.run(action, version, run))
                state = self.run("apps", version, run)
                rows.append(state)
                if (
                    rows[-2]["exit"]
                    not in ({0, 130} if action == "scratch_start" else {0})
                ) or state["output"].get("scratch_state") != expected:
                    return {
                        "evidence_kind": "trusted_lifecycle",
                        "kind": "scratch_lifecycle",
                        "version": self.versions[version]["tag"],
                        "exit": 1,
                        "steps": rows,
                        "output": {
                            "stage": action,
                            "observed": state["output"].get("scratch_state"),
                            "expected": expected,
                        },
                        "failure_identity": action
                        + str(state["output"].get("scratch_state")),
                    }
            return {
                "evidence_kind": "trusted_lifecycle",
                "kind": "scratch_lifecycle",
                "version": self.versions[version]["tag"],
                "exit": 0,
                "steps": rows,
                "output": {"start": "RUNNING", "stop": "STOPPED", "remove": "ABSENT"},
                "failure_identity": "passed",
            }
        finally:
            self.local.cleanup_scratch()

    def replay(self, probe, baseline, nightly, run, predicate="exit", field=""):
        if probe not in {
            "info",
            "cameras",
            "audio",
            "help_root",
            "help_run",
            "help_device",
            "scratch_lifecycle",
        }:
            raise ValueError("Unapproved replay recipe")
        if baseline not in {"past", "stable"} or nightly != "nightly":
            raise ValueError("Expected a stable/nightly comparison")
        if predicate not in {
            "exit",
            "invalid_json",
            "field_missing",
            "field_changed",
            "help_flag_missing",
        }:
            raise ValueError("Unapproved predicate")
        if predicate.startswith("field_") and (
            probe != "info"
            or field
            not in {
                "version",
                "osVersion",
                "cpuArchitecture",
                "hasGpu",
                "gpuArch",
                "cudaVersion",
                "jetpackVersion",
                "os",
            }
        ):
            raise ValueError("Unapproved stable device-info field")
        if predicate == "help_flag_missing" and (
            not probe.startswith("help_")
            or not re.fullmatch(r"--[a-z][a-z0-9-]{1,40}", field)
        ):
            raise ValueError("Invalid flag")
        results = [
            self.run(probe, v, run) for v in [baseline, nightly, baseline, nightly]
        ]
        b = [results[0], results[2]]
        n = [results[1], results[3]]
        if (
            self.versions[baseline]["published_at"]
            > self.versions[nightly]["published_at"]
        ):
            return {
                "verified": False,
                "reason": "Stable release is newer than nightly; chronology does not establish a regression",
                "results": results,
            }
        good = all(r["exit"] == 0 for r in b)
        bad = all(r["exit"] != 0 for r in n)
        if predicate == "invalid_json":
            good = good and all(
                r.get("json_valid", isinstance(r["output"], (dict, list))) for r in b
            )
            bad = all(
                r["exit"] == 0
                and not r.get("json_valid", not isinstance(r["output"], str))
                for r in n
            )
        elif predicate == "field_missing":
            good = good and all(
                isinstance(r["output"], dict) and field in r["output"] for r in b
            )
            bad = all(
                r["exit"] == 0
                and isinstance(r["output"], dict)
                and field not in r["output"]
                for r in n
            )
        elif predicate == "field_changed":
            good = good and all(
                isinstance(r["output"], dict) and field in r["output"] for r in b
            )
            expected = b[0]["output"].get(field) if good else None
            good = good and b[1]["output"].get(field) == expected
            bad = (
                good
                and all(
                    r["exit"] == 0
                    and isinstance(r["output"], dict)
                    and field in r["output"]
                    and r["output"][field] != expected
                    for r in n
                )
                and n[0]["output"].get(field) == n[1]["output"].get(field)
            )
        elif predicate == "help_flag_missing":
            good = good and all(field in str(r["output"]) for r in b)
            bad = all(r["exit"] == 0 and field not in str(r["output"]) for r in n)
        current = (
            [self.run(probe, "stable", run), self.run(probe, "stable", run)]
            if good and bad
            else []
        )
        newer = (
            self.versions["stable"]["published_at"]
            > self.versions["nightly"]["published_at"]
        )

        def still_fails(r):
            if predicate == "exit":
                return r["exit"] != 0
            if r["exit"] != 0:
                return None
            if predicate == "invalid_json":
                return not r.get("json_valid", not isinstance(r["output"], str))
            if predicate.startswith("field_") and not isinstance(r["output"], dict):
                return None
            if predicate == "field_missing":
                return field not in r["output"]
            if predicate == "field_changed":
                return (
                    r["output"].get(field) != expected if field in r["output"] else None
                )
            return field not in str(r["output"])

        contrast = [still_fails(r) for r in current]
        fixed = bool(newer and contrast == [False, False])
        uncertain = bool(
            newer and current and contrast not in ([False, False], [True, True])
        )
        # A baseline-working command removed or made unsupported in nightly is a
        # compatibility regression, not automatically an environment failure.
        return {
            "verified": bool(good and bad and not fixed and not uncertain),
            "current_stable_unverified": uncertain,
            "current_stable": current,
            "already_fixed_in_newer_stable": fixed,
            "kind": predicate + "-regression",
            "probe": probe,
            "field": field,
            "agent": self.agent,
            "reason": "Twice replayed baseline/nightly predicate"
            if good and bad
            else "No twice-reproduced regression",
            "results": results,
        }
