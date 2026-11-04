"""Three fixed functional experiments. No model-selected commands or expectations."""

import hashlib, json, secrets, time
from common import CANCEL, save

WORKFLOWS = ("restart_persistence", "reject_extra_args", "logs_output")


class CleanupError(RuntimeError):
    pass


def confirmed_work(experiments, summary):
    return any(
        r.get("kind") in WORKFLOWS
        and r.get("completed") is True
        and r.get("cleanup_verified") is True
        and r.get("outcome") in {"passed", "failed"}
        and isinstance(r.get("id"), str)
        and r["id"].startswith("experiment-")
        and r["id"] in summary
        for r in experiments
    )


def require_failed_experiment(experiments, kind, nightly_tag):
    if not any(
        r.get("kind") == kind
        and r.get("outcome") == "failed"
        and r.get("completed") is True
        and r.get("cleanup_verified") is True
        and r.get("version") == nightly_tag
        for r in experiments
    ):
        raise ValueError(
            "Report requires fresh failed nightly functional evidence from this session"
        )


def public_result(record):
    checks = record["output"]["checks"]
    for c in checks:
        for key in ["expected", "actual"]:
            value = c[key]
            if not (
                type(value) is bool
                or type(value) is int
                and 0 <= value <= 1000000
                or isinstance(value, str)
                and value in {"RUNNING", "STOPPED", "ABSENT", "INVALID"}
            ):
                raise ValueError("Unapproved functional observation value")
        if type(c["passed"]) is not bool or c["passed"] != (
            c["expected"] == c["actual"]
        ):
            raise ValueError("Inconsistent functional check")
        if not isinstance(c["check"], str) or not c["check"].replace("_", "").isalnum():
            raise ValueError("Invalid check identity")
    return {
        **{
            k: record[k]
            for k in [
                "id",
                "kind",
                "version",
                "agent",
                "outcome",
                "exit",
                "failure_identity",
            ]
        },
        "output": {
            "outcome": record["outcome"],
            "checks": [
                {k: c[k] for k in ["check", "expected", "actual", "passed"]}
                for c in checks
            ],
        },
        "fixture_image_digest": record.get("fixture_image_digest", ""),
        "workflow_sha256": record.get("workflow_sha256", ""),
        "steps": [
            {k: s[k] for k in ["kind", "version", "command", "exit"]}
            for s in record["steps"]
        ],
    }


def experiment(probes, kind, version, run):
    if kind not in WORKFLOWS or version not in probes.versions or not probes.local:
        raise ValueError("Unapproved functional experiment")
    local = probes.local
    started = time.monotonic()
    steps, checks = [], []
    setup = False
    fixture = {}
    reason = ""

    def check(name, expected, actual):
        checks.append(
            {
                "check": name,
                "expected": expected,
                "actual": actual,
                "passed": expected == actual,
            }
        )

    def command(name):
        result = probes.run(name, version, run)
        steps.append(result)
        return result

    def state(name, expected):
        result = command("apps")
        oracle = local.scratch_state()
        check(name + "_agent", expected, oracle)
        check(
            name + "_cli",
            expected,
            (
                result["output"].get("scratch_state", "INVALID")
                if isinstance(result["output"], dict)
                else "INVALID"
            ),
        )
        check(name + "_list_exit", 0, result["exit"])

    def start(expected_count):
        result = command("scratch_start")
        check("start_exit", True, result["exit"] in {0, 130})
        check(
            "attached_ready_output", True, result["output"].get("ready_marker", False)
        )
        until = min(time.monotonic() + 5, probes.deadline or float("inf"))
        count = local.fixture_starts()
        while (
            count != expected_count
            and time.monotonic() < until
            and not CANCEL.wait(0.1)
        ):
            count = local.fixture_starts()
        check("persistent_start_count", expected_count, count)
        state("running", "RUNNING")

    try:
        local.verify_development()
        local.reset_fixture()
        fixture = local.create_scratch() or {}
        setup = True
        start(1)
        if all(x["passed"] for x in checks):
            if kind == "restart_persistence":
                check("stop_exit", 0, command("scratch_stop")["exit"])
                state("stopped", "STOPPED")
                check("stopped_data_retained", 1, local.fixture_starts())
                if all(x["passed"] for x in checks):
                    start(2)
            elif kind == "reject_extra_args":
                check(
                    "extra_args_rejected",
                    True,
                    command("scratch_stop_extra")["exit"] != 0,
                )
                state("invalid_args_no_mutation", "RUNNING")
                check("invalid_args_preserve_data", 1, local.fixture_starts())
            else:
                logs = command("scratch_logs")
                for _ in range(2):
                    if (
                        logs["exit"] != 0
                        or logs["output"].get("ready_marker")
                        or CANCEL.wait(1)
                    ):
                        break
                    logs = command("scratch_logs")
                check("logs_exit", 0, logs["exit"])
                check(
                    "stored_ready_output",
                    True,
                    logs["output"].get("ready_marker", False),
                )
        if local.scratch_state() != "ABSENT":
            check("final_stop_exit", 0, command("scratch_stop")["exit"])
            state("final_stopped", "STOPPED")
            check("remove_exit", 0, command("scratch_remove")["exit"])
            state("removed", "ABSENT")
    except Exception as e:
        if CANCEL.is_set():
            raise
        reason = type(e).__name__
    finally:
        try:
            local.cleanup_scratch()
            if local.scratch_state() != "ABSENT":
                raise CleanupError("Owned scratch still present")
        except Exception as e:
            raise CleanupError("Owned scratch cleanup failed; testing held") from e
    failed = [x for x in checks if not x["passed"]]
    outcome = "blocked" if reason or not setup else "failed" if failed else "passed"
    result = {
        "id": "experiment-" + secrets.token_hex(8),
        "evidence_kind": "functional_workflow",
        "kind": kind,
        "version": probes.versions[version]["tag"],
        "agent": probes.agent,
        "fixture_image_digest": fixture.get("image_digest", ""),
        "workflow_sha256": hashlib.sha256(
            __import__("pathlib").Path(__file__).read_bytes()
        ).hexdigest(),
        "completed": setup and bool(steps) and not reason,
        "cleanup_verified": True,
        "outcome": outcome,
        "exit": 0 if outcome == "passed" else 1,
        "reason": reason,
        "seconds": time.monotonic() - started,
        "output": {"outcome": outcome, "checks": checks},
        "steps": steps,
        "failure_identity": hashlib.sha256(
            json.dumps(failed, sort_keys=True).encode()
        ).hexdigest(),
    }
    if run:
        save(run / (result["id"] + ".json"), result)
    return result
