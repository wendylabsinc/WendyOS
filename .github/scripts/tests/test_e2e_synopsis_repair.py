"""Offline fixtures: never use real credentials, APIs, models, or notifications."""
import copy
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
import urllib.error
import zipfile
from pathlib import Path
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("repair", Path(__file__).parents[1] / "e2e_synopsis_repair.py")
r = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(r)
REPO = "wendylabsinc/WendyOS"
GREEN, BREAKING, CURRENT = (c * 40 for c in "abc")
PATH = r.TEST_ROOT + "WendyCloudTunnelTests.swift"
OLD = "wendy cloud tunnel <local-port>:<remote-port>[/udp] [flags]"
NEW = "wendy cloud tunnel [<local-port>:<remote-port>[/udp]] [flags]"
SUITE = "WendyE2ETests.`'wendy cloud tunnel'`"
TEST = "`prints command help`()"
# Exact failure/literals and metadata shape from Ubuntu artifact
# swift-e2e-tests.gh41095365784.local-ubuntu-24.0001 (30 October 2026).
SOURCE = '''import Testing
@Suite
struct `'wendy cloud tunnel'` {
    @Test
    func `prints command help`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await cli.sh("wendy cloud tunnel --help") { result in
                let stdout = result.stdout
                #expect(result.status.isSuccess)
                #expect(stdout.contains("Usage:"))
                #expect(stdout.contains("OLD"))
                #expect(result.stderr == "")
            }
        }
    }
}
'''.replace("OLD", OLD)


def evidence(failed=True):
    import xml.etree.ElementTree as ET
    root = ET.Element("testsuite")
    case = ET.SubElement(root, "testcase", classname=SUITE, name=TEST)
    if failed:
        ET.SubElement(case, "failure", message=f'Expectation failed: stdout.contains("{OLD}") (error)')
    return {"test-results.xml": ET.tostring(root, encoding="unicode"),
            "observations/wendy-cloud-tunnel/prints-command-help/test.json": json.dumps({
                "schema": "wendy.e2e.test.v1", "suiteName": "'wendy cloud tunnel'",
                "functionName": "prints command help()", "sourceStartLine": 4, "sourceEndLine": 15,
                "sourceFilePath": "Tests/WendyE2ETests/WendyCloudTunnelTests.swift"}),
            "observations/wendy-cloud-tunnel/prints-command-help/recording.md":
                f"## Command\n\n- Command: `wendy cloud tunnel --help`\n"
                f"- Termination status: `exited(0)`\n\n### stdout\n\n```text\nUsage:\n  {NEW}\n\n```\n"
                "\n### stderr\n\n```text\n\n```\n"}


def run(number, sha):
    return {"id": number, "head_sha": sha, "event": "push", "head_branch": "main",
            "head_repository": {"full_name": REPO}, "path": ".github/workflows/swift-e2e-tests.yml",
            "status": "completed", "run_attempt": 1, "conclusion": "success" if number == 1 else "failure"}


class API:
    repo = REPO

    def __init__(self):
        self.runs = [run(3, CURRENT), run(2, BREAKING), run(1, GREEN)]
        self.prs, self.refs, self.writes = [], [], []
        self.main = CURRENT
        self.fail_write = None
        self.verification_status = "success"
        self.calls = []

    def source(self, sha, path):
        assert path == PATH
        return SOURCE

    def artifact(self, source_run):
        return evidence(source_run["id"] != 1)

    def get(self, path):
        self.calls.append(path)
        if path == "actions/runs/99":
            return {"event": "workflow_run", "head_branch": "main", "run_attempt": 1,
                    "path": ".github/workflows/e2e-synopsis-repair.yml"}
        if path == "actions/runs/99/attempts/1/jobs?per_page=100":
            return {"total_count": 1, "jobs": [{"name": "verify", "conclusion": self.verification_status}]}
        for source_run in self.runs:
            if path == f"actions/runs/{source_run['id']}":
                return source_run
            if path == f"actions/runs/{source_run['id']}/attempts/1/jobs?per_page=100":
                return {"total_count": 2, "jobs": [
                    {"name": "Local: Ubuntu 24", "conclusion": source_run["conclusion"]},
                    {"name": "Local: macOS 26", "conclusion": "skipped"}]}
        if path == "git/ref/heads/main":
            return {"object": {"sha": self.main}}
        if path.startswith("actions/workflows/"):
            return {"workflow_runs": self.runs}
        if path == f"commits/{CURRENT}":
            return {"parents": [{"sha": BREAKING}, {"sha": "d" * 40}]}
        if path == f"commits/{BREAKING}":
            return {"parents": [{"sha": GREEN}, {"sha": "e" * 40}]}
        if path == f"commits/{BREAKING}/pulls?per_page=100":
            return [{"number": 12, "title": "Allow optional port mapping", "merged_at": "2026-10-31T01:00:00Z",
                     "merge_commit_sha": BREAKING, "base": {"ref": "main"},
                     "head": {"ref": "feature/tunnel-help", "repo": {"full_name": REPO}},
                     "user": {"login": "author", "type": "User"}}]
        if path.startswith("pulls?"):
            assert "state=all" in path and "head=" in path
            return self.prs
        if path.startswith("git/matching-refs/"):
            return self.refs
        if path == f"git/commits/{CURRENT}":
            return {"tree": {"sha": "f" * 40}}
        raise AssertionError(f"Unexpected API call: {path}")

    def post(self, path, body):
        self.writes.append((path, body))
        if path == self.fail_write:
            raise urllib.error.URLError("ambiguous timeout")
        if path == "git/refs":
            self.refs.append({"ref": body["ref"]})
        if path == "pulls":
            self.prs.append({"number": 100, "body": body["body"]})
            return self.prs[-1]
        return {"sha": "f" * 40}


class RepairTests(unittest.TestCase):
    def test_real_failure_shape_changes_only_the_literal(self):
        change = r.candidate(evidence(), lambda p: SOURCE)
        self.assertEqual(change["new"], NEW)
        self.assertEqual(r.replace_literal(SOURCE, change), SOURCE.replace(OLD, NEW))

    def test_malformed_or_disallowed_evidence(self):
        for transform in (
            lambda f: f.update({"test-results.xml": "<!DOCTYPE x><testsuite/>"}),
            lambda f: f.update({"test-results.xml": evidence(False)["test-results.xml"]}),
            lambda f: f.update({"test-results.xml": f["test-results.xml"].replace("prints command help", "deletes files")}),
            lambda f: f.update({k: v.replace(NEW, "wendy cloud tunnel [flags]") for k, v in f.items()}),
            lambda f: f.update({k: v.replace("exited(0)", "exited(1)") for k, v in f.items()}),
            lambda f: f.update({k: v.replace("Tests/WendyE2ETests/WendyCloudTunnelTests.swift", "../../.github/workflows/pwn.yml") for k, v in f.items()}),
            lambda f: f.update({k: v.replace("wendy cloud tunnel --help", "curl bad.example") for k, v in f.items()}),
            lambda f: f.update({k: v.replace('"sourceEndLine": 15', '"sourceEndLine": 1000') for k, v in f.items()}),
        ):
            with self.subTest(transform=transform):
                files = evidence()
                transform(files)
                with self.assertRaises(r.Skip):
                    r.candidate(files, lambda p: SOURCE)

    def test_source_correlation_is_required(self):
        for source in (SOURCE.replace("stdout.contains", "stderr.contains"),
                       SOURCE.replace("authenticated: false", "authenticated: true"),
                       SOURCE.replace("#expect(stdout.contains", "#expect(!stdout.contains"),
                       SOURCE + '\nlet duplicate = "' + OLD + '"'):
            with self.subTest(source=source), self.assertRaises(r.Skip):
                r.candidate(evidence(), lambda p, source=source: source)

    def test_disallowed_patch_payload_and_stale_source(self):
        change = r.candidate(evidence(), lambda p: SOURCE)
        for new in ('")); fatalError("oops', "", "wendy cloud tunnel [flags]"):
            with self.subTest(new=new), self.assertRaises(r.Skip):
                r.replace_literal(SOURCE, {**change, "new": new})
        with self.assertRaises(r.Skip):
            r.replace_literal(SOURCE.replace(OLD, NEW), change)

    def test_chain_finds_first_bad_not_latest_merge(self):
        proposal = r.prepare(API(), 3)
        self.assertEqual(proposal["breaking"], BREAKING)
        self.assertEqual(proposal["failure_run"], 2)
        self.assertEqual(proposal["green_run"], 1)
        self.assertEqual(proposal["pr"], 12)

    def test_out_of_order_run_listing_is_not_attribution_order(self):
        api = API()
        api.runs.reverse()
        self.assertEqual(r.prepare(api, 3)["breaking"], BREAKING)

    def test_incomplete_cancelled_pending_rerun_or_fork_history_skips(self):
        for field, value in (("status", "in_progress"), ("conclusion", "cancelled"),
                             ("run_attempt", 2), ("event", "pull_request"),
                             ("head_repository", {"full_name": "attacker/WendyOS"})):
            api = API()
            api.runs[1][field] = value
            with self.subTest(field=field), self.assertRaises(r.Skip):
                r.prepare(api, 3)
            self.assertFalse(api.writes)
        for remove in (0, 1, 2):
            api = API()
            api.runs.pop(remove)
            if remove == 0:
                continue  # source run endpoint itself is absent
            with self.assertRaises(r.Skip):
                r.prepare(api, 3)

    def test_bounded_history(self):
        with patch.object(r, "MAX_COMMITS", 2), self.assertRaises(r.Skip):
            r.prepare(API(), 3)

    def test_different_failure_during_red_chain_skips(self):
        api = API()
        original = api.artifact
        api.artifact = lambda run: evidence(False) if run["id"] == 2 else original(run)
        with self.assertRaises(r.Skip):
            r.prepare(api, 3)

    def test_test_source_changed_since_green_skips(self):
        api = API()
        api.source = lambda sha, p: SOURCE + ("\n// changed" if sha == GREEN else "")
        with self.assertRaises(r.Skip):
            r.prepare(api, 3)

    def test_existing_open_closed_pr_and_orphan_branch_skip(self):
        for state in ("open", "closed"):
            api = API()
            api.prs = [{"number": 100, "state": state}]
            with self.subTest(state=state), self.assertRaisesRegex(r.Skip, "Existing fix PR"):
                r.prepare(api, 3)
            self.assertFalse(api.writes)
        api = API()
        api.refs = [{"ref": "refs/heads/" + r.branch(BREAKING)}]
        with self.assertRaisesRegex(r.Skip, "Existing repair branch"):
            r.prepare(api, 3)

    def test_stale_main_skips(self):
        api = API()
        api.main = GREEN
        with self.assertRaisesRegex(r.Skip, "Main has advanced"):
            r.prepare(api, 3)
        self.assertFalse(api.writes)

    def publish(self, api, proposal=None, mapping=None, slack=True):
        proposal = proposal or r.prepare(api, 3)
        return r.publish(api, proposal, r.verification(proposal, 99, 1), 99, 1,
                         mapping or {"author": "U123456789"}, slack)

    def test_create_only_draft_and_notification_reservation(self):
        api = API()
        payload = self.publish(api)
        self.assertIn("<@U123456789>", payload["text"])
        self.assertEqual([p for p, _ in api.writes],
                         ["git/blobs", "git/trees", "git/commits", "git/refs", "pulls", "issues/12/comments"])
        body = dict(api.writes)["pulls"]
        self.assertTrue(body["draft"])
        self.assertIn("notification-attempts: reserved", body["body"])
        self.assertEqual(dict(api.writes)["git/commits"]["parents"], [CURRENT])
        self.assertEqual(dict(api.writes)["git/blobs"]["content"], SOURCE.replace(OLD, NEW))

    def test_no_repeat_after_github_notification_timeout(self):
        api = API()
        proposal = r.prepare(api, 3)
        api.fail_write = "issues/12/comments"
        with self.assertRaises(urllib.error.URLError):
            self.publish(api, proposal)
        before = len(api.writes)
        with self.assertRaisesRegex(r.Skip, "Existing fix PR"):
            self.publish(api, proposal)
        self.assertEqual(len(api.writes), before)

    def test_no_repeat_after_slack_failure_or_success(self):
        api = API()
        proposal = r.prepare(api, 3)
        self.publish(api, proposal)  # Slack action may succeed, fail, or time out after this.
        before = len(api.writes)
        with self.assertRaisesRegex(r.Skip, "Existing fix PR"):
            self.publish(api, proposal)
        self.assertEqual(len(api.writes), before)

    def test_no_retry_after_branch_created_but_pr_failed(self):
        api = API()
        proposal = r.prepare(api, 3)
        api.fail_write = "pulls"
        with self.assertRaises(urllib.error.URLError):
            self.publish(api, proposal)
        with self.assertRaisesRegex(r.Skip, "Existing repair branch"):
            self.publish(api, proposal)

    def test_missing_mapping_reports_no_ping(self):
        api = API()
        payload = self.publish(api, mapping={"another": "U123456789"})
        self.assertNotIn("<@", payload["text"])
        self.assertIn("not pinged", payload["text"])
        self.assertIn("mapping is missing", dict(api.writes)["pulls"]["body"])

    def test_missing_slack_does_not_claim_delivery(self):
        api = API()
        self.assertIsNone(self.publish(api, slack=False))
        self.assertIn("will not be attempted", dict(api.writes)["pulls"]["body"])

    def test_verification_digest_sha_attempt_and_job_status(self):
        api = API()
        proposal = r.prepare(api, 3)
        proof = r.verification(proposal, 99, 1)
        for key, value in (("candidate_sha256", "bad"), ("tested_sha", GREEN),
                           ("repair_run", 98), ("repair_attempt", 2), ("source_run", 2)):
            with self.subTest(key=key), self.assertRaises(r.Skip):
                r.publish(api, proposal, {**proof, key: value}, 99, 1, {}, True)
        api.verification_status = "failure"
        with self.assertRaises(r.Skip):
            self.publish(api, proposal)
        self.assertFalse(api.writes)

    def test_candidate_tampered_after_verification(self):
        api = API()
        proposal = r.prepare(api, 3)
        proof = r.verification(proposal, 99, 1)
        tampered = copy.deepcopy(proposal)
        tampered["change"]["new"] = "wendy cloud tunnel [flags]"
        with self.assertRaises(r.Skip):
            r.publish(api, tampered, proof, 99, 1, {}, True)
        self.assertFalse(api.writes)

    def test_channel_ids_and_existing_channel_name(self):
        for value in ("#feed-e2e", "C0B92LJPVK2", "G123456789"):
            self.assertTrue(r.valid_channel(value))
        for value in ("U123456789", "#feed-e2e\nfoo", "<!channel>", "#name with spaces", ""):
            self.assertFalse(r.valid_channel(value))

    def test_filter_obeys_harness_and_matches_quoted_test_ids(self):
        change = r.candidate(evidence(), lambda p: SOURCE)
        pattern = r.test_filter(change)
        self.assertRegex(pattern, r"^[A-Za-z0-9][A-Za-z0-9 ._:/|(),_-]*$")
        for specifier in ("WendyE2ETests.'wendy cloud tunnel'/prints command help()",
                          "WendyE2ETests.`'wendy cloud tunnel'`/`prints command help`()"):
            self.assertRegex(specifier, pattern)

    def test_archive_provenance_and_duplicate_entries(self):
        api = r.GitHub(REPO, "fixture-not-a-token")
        manifest = {"git": {"commit": CURRENT, "dirty": False},
                    "github": {"repository": REPO, "runID": "3", "runAttempt": "1"},
                    "test": {"filters": ["WendyE2ETests"]}}
        api.get = lambda path: {"total_count": 1, "artifacts": [{"id": 1, "expired": False,
            "name": "swift-e2e-tests.gh3.local-ubuntu-24.0001", "size_in_bytes": 123}]}

        def archive(data):
            stream = io.BytesIO()
            with zipfile.ZipFile(stream, "w") as z:
                for name, text in data.items():
                    z.writestr(name, text)
            return stream.getvalue()

        api.request = lambda path, binary: archive({**evidence(), "attempt.json": json.dumps(manifest)})
        self.assertEqual(r.candidate(api.artifact(run(3, CURRENT)), lambda p: SOURCE)["new"], NEW)
        for key, value in (("runID", "4"), ("runAttempt", "2"), ("repository", "fork/other")):
            bad = copy.deepcopy(manifest)
            bad["github"][key] = value
            api.request = lambda path, binary, bad=bad: archive({**evidence(), "attempt.json": json.dumps(bad)})
            with self.subTest(key=key), self.assertRaises(r.Skip):
                api.artifact(run(3, CURRENT))

    def test_apply_and_proof_check_in_temporary_git_checkout(self):
        proposal = r.prepare(API(), 3)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / PATH
            path.parent.mkdir(parents=True)
            path.write_text(SOURCE)

            def git(*args):
                return subprocess.check_output(["git", "-C", directory, "-c", "user.name=Fixture",
                    "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false",
                    "-c", "core.hooksPath=/dev/null", *args], text=True, stderr=subprocess.DEVNULL)

            git("init")
            git("add", ".")
            git("commit", "-m", "fixture")
            proposal["sha"] = git("rev-parse", "HEAD").strip()
            proposal_path = root / "proposal.json"
            proposal_path.write_text(json.dumps(proposal))
            result_path, proof_path = root / "results.xml", root / "proof.json"
            result_path.write_text(evidence(False)["test-results.xml"])
            argv = ["repair", "apply", "--source", directory, "--proposal", str(proposal_path)]
            with patch.object(sys, "argv", argv), patch.dict(os.environ, {"GITHUB_OUTPUT": ""}):
                r.main()
            self.assertEqual(path.read_text(), SOURCE.replace(OLD, NEW))
            argv[1] = "check"
            argv += ["--results", str(result_path), "--proof", str(proof_path)]
            with patch.object(sys, "argv", argv), patch.dict(os.environ, {"GITHUB_RUN_ID": "99", "GITHUB_RUN_ATTEMPT": "1"}):
                r.main()
                self.assertEqual(json.loads(proof_path.read_text()), r.verification(proposal, 99, 1))
                for xml in ("<testsuite/>", evidence()["test-results.xml"],
                            evidence(False)["test-results.xml"].replace("</testsuite>", "<testcase/></testsuite>")):
                    result_path.write_text(xml)
                    with self.assertRaises(SystemExit):
                        r.main()
                path.write_text(path.read_text() + "\n// extra change")
                with self.assertRaises(SystemExit):
                    r.main()

    def test_credential_not_forwarded_to_artifact_host(self):
        req = r.urllib.request.Request("https://api.github.com/artifacts", headers={"Authorization": "Bearer secret"})
        redirected = r.NoAuthRedirect().redirect_request(req, None, 302, "", {}, "https://artifact.example/file")
        self.assertIsNone(redirected.get_header("Authorization"))


if __name__ == "__main__":
    unittest.main()
