import copy, json, pathlib, sys, tempfile, unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "control"))
from functional import (
    model_observation,
    experiment,
    confirmed_work,
    CleanupError,
    require_failed_experiment,
)


class Fixture:
    def __init__(self):
        self.state = "ABSENT"
        self.starts = 0
        self.cleanup_bad = False

    def verify_development(self):
        return {"environment": "development"}

    def cleanup_scratch(self):
        if self.cleanup_bad:
            raise RuntimeError("fixture cleanup failure")
        self.state = "ABSENT"

    def reset_fixture(self):
        self.cleanup_scratch()
        self.starts = 0

    def create_scratch(self):
        self.state = "STOPPED"

    def scratch_state(self):
        return self.state

    def fixture_starts(self):
        return self.starts


class Runner:
    def __init__(self, broken=""):
        self.local = Fixture()
        self.versions = {k: {"tag": k} for k in ["past", "stable", "nightly"]}
        self.agent = "fixture-agent"
        self.deadline = None
        self.broken = broken
        self.calls = []

    def run(self, name, version, run):
        self.calls.append(name)
        f = self.local
        code, output = 0, {}
        if name == "scratch_start":
            f.state = "RUNNING"
            f.starts = 1 if self.broken == "persistence" else f.starts + 1
            output = {"ready_marker": True}
        elif name == "scratch_stop":
            f.state = "STOPPED"
        elif name == "scratch_remove":
            f.state = "ABSENT"
        elif name == "scratch_stop_extra":
            code = 2
            if self.broken == "args":
                f.state = "STOPPED"
            elif self.broken == "multi-target":
                f.state, code = "STOPPED", 0
        elif name == "scratch_logs":
            output = {"ready_marker": self.broken != "logs"}
        elif name == "apps":
            output = {
                "scratch_state": "STOPPED" if self.broken == "listing" else f.state
            }
        return {
            "kind": name,
            "version": version,
            "exit": code,
            "output": output,
            "command": ["wendy", "<Scout-owned scratch ID>"],
        }


class FunctionalTests(unittest.TestCase):
    def test_distinct_workflows_check_state_and_cleanup(self):
        for kind in ["restart_persistence", "reject_extra_args", "logs_output"]:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                p = Runner()
                r = experiment(p, kind, "stable", pathlib.Path(tmp))
                self.assertEqual(r["outcome"], "passed")
                self.assertTrue(r["completed"] and r["cleanup_verified"])
                self.assertEqual(p.local.state, "ABSENT")
                self.assertEqual(
                    json.loads(next(pathlib.Path(tmp).glob("*.json")).read_text())[
                        "id"
                    ],
                    r["id"],
                )

    def test_real_invariants_fail_without_treating_any_json_change_as_bug(self):
        for broken, kind in [
            ("persistence", "restart_persistence"),
            ("args", "reject_extra_args"),
            ("logs", "logs_output"),
            ("listing", "restart_persistence"),
        ]:
            with self.subTest(broken=broken):
                p = Runner(broken)
                r = experiment(p, kind, "nightly", None)
                self.assertEqual(r["outcome"], "failed")
                self.assertTrue(any(not c["passed"] for c in r["output"]["checks"]))
                self.assertEqual(p.local.state, "ABSENT")

    def test_cleanup_failure_holds_and_does_not_become_publishable(self):
        p = Runner()
        p.local.cleanup_bad = True
        with self.assertRaises(CleanupError):
            experiment(p, "restart_persistence", "stable", None)

    def test_successful_argument_contract_extension_is_not_automatically_a_bug(self):
        r = experiment(Runner("multi-target"), "reject_extra_args", "nightly", None)
        self.assertEqual(r["outcome"], "blocked")
        self.assertTrue(r["cleanup_verified"])
        with self.assertRaises(ValueError):
            require_failed_experiment([r], "reject_extra_args", "nightly")

    def test_unknown_workflow_and_prod_rejected_before_actions(self):
        p = Runner()
        for kind, version in [("shell", "stable"), ("restart_persistence", "prod")]:
            with self.assertRaises(ValueError):
                experiment(p, kind, version, None)
        self.assertEqual(p.calls, [])

    def test_fresh_work_proof_rejects_hallucinated_or_historical_ids(self):
        r = experiment(Runner(), "restart_persistence", "stable", None)
        self.assertTrue(confirmed_work([r], "Checked " + r["id"]))
        self.assertFalse(
            confirmed_work([], "I executed everything experiment-invented")
        )
        self.assertFalse(confirmed_work([r], "I ran commands but supply no fresh ID"))
        for key in ["completed", "cleanup_verified"]:
            bad = copy.deepcopy(r)
            bad[key] = False
            self.assertFalse(confirmed_work([bad], bad["id"]))

    def test_report_requires_current_failed_nightly_not_model_claims(self):
        good = experiment(Runner(), "restart_persistence", "nightly", None)
        for rows in [[], [good], [{**good, "outcome": "failed", "completed": False}]]:
            with self.assertRaises(ValueError):
                require_failed_experiment(rows, "restart_persistence", "nightly")
        failed = experiment(Runner("args"), "reject_extra_args", "nightly", None)
        require_failed_experiment([failed], "reject_extra_args", "nightly")
        with self.assertRaises(ValueError):
            require_failed_experiment([failed], "restart_persistence", "nightly")

    def test_model_gets_checked_evidence_without_repeating_private_trace(self):
        r = experiment(Runner(), "restart_persistence", "stable", None)
        for step in r["steps"]:
            step["raw_private_stdout"] = "private" * 1000
        compact = model_observation(r)
        self.assertTrue(compact["completed"] and compact["cleanup_verified"])
        self.assertEqual(compact["id"], r["id"])
        self.assertNotIn("raw_private_stdout", json.dumps(compact))
        self.assertEqual(
            compact["output"]["check_columns"], ["check", "expected", "actual"]
        )
        self.assertEqual(
            compact["output"]["checks"],
            [[c["check"], c["expected"], c["actual"]] for c in r["output"]["checks"]],
        )
        self.assertLess(len(json.dumps(compact)), 3000)
