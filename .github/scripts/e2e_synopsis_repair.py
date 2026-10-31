"""Create-only, bounded CLI synopsis repairs. Artifacts are data, never commands."""
import argparse
import base64
import hashlib
import io
import json
import os
import re
import subprocess
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

WORKFLOW = "swift-e2e-tests.yml"
MAX_COMMITS = 20
MAX_BYTES = 32 * 1024 * 1024
TEST_ROOT = "swift/WendyE2ETests/Tests/WendyE2ETests/"
SHA = re.compile(r"[0-9a-f]{40}")


class Skip(Exception):
    """Insufficient evidence: no writes, no attribution."""


def require(condition, reason):
    if not condition:
        raise Skip(reason)


class NoAuthRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if redirected:
            require(urllib.parse.urlparse(newurl).scheme == "https", "Non-HTTPS redirect")
            redirected.remove_header("Authorization")
        return redirected


class GitHub:
    def __init__(self, repo, token, writer_token=None):
        require(re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo), "Invalid repository")
        self.repo, self.token = repo, token
        self.writer_token = writer_token

    def request(self, path, data=None, binary=False):
        req = urllib.request.Request(
            f"https://api.github.com/repos/{self.repo}/{path}",
            data=None if data is None else json.dumps(data).encode(),
            headers={"Authorization": f"Bearer {self.token if data is None else self.writer_token}",
                     "Accept": "application/vnd.github+json", "Content-Type": "application/json",
                     "X-GitHub-Api-Version": "2026-03-10"},
        )
        # No automatic retries of writes, including ambiguous network failures.
        with urllib.request.build_opener(NoAuthRedirect()).open(req, timeout=45) as response:
            raw = response.read(MAX_BYTES + 1)
        require(len(raw) <= MAX_BYTES, "API response exceeds size limit")
        return raw if binary else json.loads(raw)

    def get(self, path):
        return self.request(path)

    def post(self, path, data):
        require(self.writer_token, "Dedicated writer credential is missing")
        return self.request(path, data)

    def source(self, sha, path):
        require(SHA.fullmatch(sha), "Invalid source SHA")
        obj = self.get(f"contents/{path}?ref={sha}")
        require(obj.get("type") == "file" and obj.get("encoding") == "base64", "Not a regular source file")
        return base64.b64decode(obj["content"], validate=False).decode("utf-8")

    def artifact(self, run):
        name = f"swift-e2e-tests.gh{run['id']}.local-ubuntu-24.{run['run_attempt']:04d}"
        listing = self.get(f"actions/runs/{run['id']}/artifacts?per_page=100")
        require(listing["total_count"] <= 100, "Too many source artifacts")
        matches = [a for a in listing["artifacts"] if a["name"] == name and not a["expired"]]
        require(len(matches) == 1 and matches[0]["size_in_bytes"] <= MAX_BYTES, "Missing/ambiguous Ubuntu artifact")
        raw = self.request(f"actions/artifacts/{matches[0]['id']}/zip", binary=True)
        with zipfile.ZipFile(io.BytesIO(raw)) as archive:
            entries = archive.infolist()
            names = [e.filename for e in entries]
            require(len(names) == len(set(names)) and len(names) <= 10000, "Ambiguous artifact entries")
            require(sum(e.file_size for e in entries) <= MAX_BYTES, "Artifact exceeds size limit")
            # Do not extract anything. Only bounded UTF-8 evidence files are read.
            files = {n: archive.read(n).decode("utf-8") for n in names
                     if n in ("attempt.json", "test-results.xml") or
                     re.fullmatch(r"observations/[a-z0-9-]+/prints-command-help/(test.json|recording.md)", n)}
        manifest = json.loads(files.get("attempt.json", "{}"))
        require(manifest.get("git", {}).get("commit") == run["head_sha"] and
                manifest.get("git", {}).get("dirty") is False and
                manifest.get("github", {}).get("repository") == self.repo and
                manifest.get("github", {}).get("runID") == str(run["id"]) and
                manifest.get("github", {}).get("runAttempt") == str(run["run_attempt"]) and
                manifest.get("test", {}).get("filters") == ["WendyE2ETests"], "Artifact provenance/full-suite mismatch")
        return files


def cases(files):
    xml = files.get("test-results.xml", "")
    require(xml and "<!DOCTYPE" not in xml and "<!ENTITY" not in xml, "Missing/unsafe test results")
    return list(ET.fromstring(xml).iter("testcase"))


def identity(case):
    return {"suite": case.get("classname"), "test": case.get("name")}


def verify_results(change, xml):
    results = cases({"test-results.xml": xml})
    require(len(results) == 1 and identity(results[0]) == {k: change[k] for k in ('suite', 'test')} and
            not list(results[0]), "Verification did not pass exactly the failed test")


def candidate(files, source):
    failures = [(c, f) for c in cases(files) for f in c.findall("failure") + c.findall("error")]
    require(len(failures) == 1, "Expected exactly one failing assertion")
    case, failure = failures[0]
    require(case.get("name") == "`prints command help`()", "Not a help test")
    match = re.fullmatch(r'Expectation failed: stdout\.contains\("([^"\n]+)"\) \(error\)',
                         failure.get("message") or failure.text or "")
    require(match is not None, "Not a literal stdout.contains failure")
    old = match[1]
    matches = []
    for path, text in files.items():
        if not path.endswith("/test.json"):
            continue
        meta = json.loads(text)
        if (meta.get("schema") == "wendy.e2e.test.v1" and
                case.get("classname") == f"WendyE2ETests.`{meta.get('suiteName')}`" and
                meta.get("functionName") == "prints command help()"):
            matches.append((path, meta))
    require(len(matches) == 1, "Missing/ambiguous source correlation")
    path, meta = matches[0]
    source_path = "swift/WendyE2ETests/" + meta["sourceFilePath"]
    require(re.fullmatch(re.escape(TEST_ROOT) + r"Wendy[A-Za-z0-9]+Tests\.swift", source_path), "Disallowed source path")
    command_match = re.fullmatch(r"'(wendy(?: [a-z][a-z0-9-]*)+)'", meta["suiteName"])
    require(command_match is not None, "Not a literal CLI command suite")
    command = command_match[1]
    recording = files.get(path.removesuffix("test.json") + "recording.md", "")
    require(recording.count("## Command\n") == 1 and
            f"- Command: `{command} --help`\n" in recording and
            "- Termination status: `exited(0)`\n" in recording and
            "### stderr\n\n```text\n\n```" in recording, "Help command/result correlation failed")
    stdout = re.findall(r"### stdout\n\n```text\n(.*?)\n```", recording, re.DOTALL)
    require(len(stdout) == 1, "Ambiguous stdout")
    usage = re.findall(r"^Usage:\n  ([^\n]+)\n", stdout[0], re.MULTILINE)
    require(len(usage) == 1 and old not in stdout[0], "No unique changed synopsis")
    new = usage[0]
    text = source(source_path)
    start, end = meta["sourceStartLine"], meta["sourceEndLine"]
    require(type(start) is int and type(end) is int and 1 <= start <= end <= len(text.splitlines()), "Invalid source range")
    scope = "\n".join(text.splitlines()[start - 1:end])
    expression = r'#expect\(\s*stdout\.contains\(\s*"' + re.escape(old) + r'"\s*\)\s*\)'
    require('func `prints command help`()' in scope and
            f'cli.sh("{command} --help")' in scope and
            'scenario.run(authenticated: false)' in scope and
            re.search(expression, scope), "Failed assertion not in recorded help test")
    result = {"path": source_path, "old": old, "new": new, "command": command, **identity(case)}
    replace_literal(text, result)
    return result


def replace_literal(text, change):
    # No arbitrary patch input: one exact quoted literal in one approved file.
    for literal in (change["old"], change["new"]):
        require(len(literal) <= 240 and re.fullmatch(r"[A-Za-z0-9 <>:/._|()\[\]-]+", literal) and
                literal.startswith(change["command"] + " ") and literal.endswith(" [flags]"), "Disallowed literal")
    require(re.fullmatch(r"wendy(?: [a-z][a-z0-9-]*)+", change["command"]) and
            change["old"] != change["new"] and
            re.sub(r"[\[\]]", "", change["old"]).split() ==
            re.sub(r"[\[\]]", "", change["new"]).split(), "Disallowed edit")
    require(text.count('"' + change["old"] + '"') == 1, "Stale/ambiguous source literal")
    return text.replace('"' + change["old"] + '"', '"' + change["new"] + '"', 1)


def outcome(api, run):
    require(run.get("event") == "push" and run.get("head_branch") == "main" and
            run.get("head_repository", {}).get("full_name") == api.repo and
            run.get("path") == ".github/workflows/" + WORKFLOW and
            run.get("status") == "completed" and run.get("run_attempt") == 1 and
            run.get("conclusion") not in ("cancelled", "timed_out", "action_required", "stale"),
            "Missing, pending, cancelled, rerun or non-main evidence")
    jobs = api.get(f"actions/runs/{run['id']}/attempts/1/jobs?per_page=100")
    require(jobs["total_count"] <= 100, "Too many jobs")
    ubuntu = [j for j in jobs["jobs"] if j["name"] == "Local: Ubuntu 24"]
    mac = [j for j in jobs["jobs"] if j["name"] == "Local: macOS 26"]
    require(len(ubuntu) == len(mac) == 1 and mac[0]["conclusion"] in ("success", "skipped"),
            "Ambiguous jobs or a separate macOS failure")
    status = ubuntu[0]["conclusion"]
    require(status in ("success", "failure"), "Ubuntu result is incomplete")
    return status


def marker(sha):
    require(SHA.fullmatch(sha), "Invalid breaking SHA")
    return f"<!-- e2e-synopsis-repair:{sha} -->"


def branch(sha):
    marker(sha)
    return "ai/e2e-synopsis/" + sha


def existing(api, sha):
    # state=all includes closed/merged drafts; a closed fix is never reopened.
    query = urllib.parse.urlencode({"state": "all", "head": api.repo.split('/')[0] + ':' + branch(sha), "per_page": 100})
    prs = api.get("pulls?" + query)
    require(len(prs) < 100, "Ambiguous existing repair lookup")
    if prs:
        pr = prs[0]
        return (f"Existing fix PR [{api.repo} #{pr['number']} — {title(pr['title'])}]"
                f"(https://github.com/{api.repo}/pull/{pr['number']}); no new draft or notifications")
    refs = api.get("git/matching-refs/heads/" + branch(sha))
    if any(r["ref"] == "refs/heads/" + branch(sha) for r in refs):
        return "Existing repair branch; manual follow-up, never rewrite"
    return None


def prepare(api, run_id):
    run = api.get(f"actions/runs/{run_id}")
    require(outcome(api, run) == "failure", "Source E2E is not a supported failure")
    require(api.get("git/ref/heads/main")["object"]["sha"] == run["head_sha"], "Main has advanced; skip stale run")
    listing = api.get(f"actions/workflows/{WORKFLOW}/runs?branch=main&event=push&per_page=100")
    runs = listing["workflow_runs"]
    change = candidate(api.artifact(run), lambda p: api.source(run["head_sha"], p))
    sha, first_bad, green = run["head_sha"], None, None
    for _ in range(MAX_COMMITS):
        matching = [r for r in runs if r["head_sha"] == sha]
        require(len(matching) == 1, "Incomplete/ambiguous first-parent run history")
        current = matching[0]
        status = outcome(api, current)
        if status == "success":
            passing = [c for c in cases(api.artifact(current)) if identity(c) == {k: change[k] for k in ('suite', 'test')}]
            require(len(passing) == 1 and not list(passing[0]), "Green run did not pass this exact test")
            green = current
            break
        observed = candidate(api.artifact(current), lambda p, revision=sha: api.source(revision, p))
        require(observed == change, "Red chain contains a different failure")
        first_bad = current
        commit = api.get(f"commits/{sha}")
        require(commit["parents"], "No first parent")
        sha = commit["parents"][0]["sha"]
    require(green is not None and first_bad is not None, "No preceding green within 20 first-parent commits")
    breaking = first_bad["head_sha"]
    require(api.source(green["head_sha"], change["path"]) == api.source(breaking, change["path"]),
            "Test source changed across the green-to-red interval")
    merge = api.get(f"commits/{breaking}")
    require(len(merge["parents"]) == 2, "First failing commit is not a merge")
    prs = api.get(f"commits/{breaking}/pulls?per_page=100")
    linked = [p for p in prs if p.get("merged_at") and p.get("merge_commit_sha") == breaking and p["base"]["ref"] == "main"]
    require(len(prs) < 100 and len(linked) == 1, "Cannot establish one breaking PR")
    pr = linked[0]
    require(pr["user"]["type"] == "User" and
            re.fullmatch(r"[A-Za-z0-9-]{1,39}", pr["user"]["login"]) and
            (pr["head"].get("repo") or {}).get("full_name") == api.repo and
            not pr["head"]["ref"].startswith(("ai/", "dependabot/", "renovate/")), "Bot/fork repair attribution excluded")
    duplicate = existing(api, breaking)
    require(duplicate is None, duplicate)
    return {"source_run": run_id, "sha": run["head_sha"], "breaking": breaking,
            "failure_run": first_bad["id"], "green_run": green["id"], "pr": pr["number"],
            "author": pr["user"]["login"], "pr_title": pr["title"], "change": change}


def title(text):
    return re.sub(r"[^\w .,:()/-]", "", text)[:180]


def verification(proposal, run_id, attempt):
    return {"candidate_sha256": hashlib.sha256(json.dumps(proposal, sort_keys=True).encode()).hexdigest(),
            "tested_sha": proposal["sha"], "source_run": proposal["source_run"],
            "repair_run": run_id, "repair_attempt": attempt}


def verify_publication(api, proposal, proof, run_id, attempt):
    require(proof == verification(proposal, run_id, attempt), "Verification does not bind this exact candidate/run")
    run = api.get(f"actions/runs/{run_id}")
    require(run["event"] == "workflow_run" and run["head_branch"] == "main" and
            run["path"] == ".github/workflows/e2e-synopsis-repair.yml" and
            run["run_attempt"] == attempt, "Not the expected repair workflow attempt")
    jobs = api.get(f"actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100")
    verified = [j for j in jobs["jobs"] if j["name"] == "verify"]
    require(jobs["total_count"] <= 100 and len(verified) == 1 and verified[0]["conclusion"] == "success",
            "Exact verification job has not succeeded")


def valid_channel(channel):
    return bool(re.fullmatch(r"[CG][A-Z0-9]{8,}|#[a-z0-9][a-z0-9_-]{0,79}", channel))


def test_filter(change):
    # SwiftPM/Swift Testing versions quote unusual identifiers differently.
    # The harness permits bounded alternatives, not '*'. check requires exactly
    # one successful XUnit case with the original suite/function identity.
    return change["command"] + "(.|..|...|....)prints command help"


def checked_source(proposal, root):
    require(SHA.fullmatch(proposal["sha"]), "Invalid tested SHA")
    change = proposal["change"]
    require(re.fullmatch(re.escape(TEST_ROOT) + r"Wendy[A-Za-z0-9]+Tests\.swift", change["path"]), "Disallowed path")
    path = Path(root) / change["path"]
    require(not path.is_symlink() and path.resolve().is_relative_to(Path(root).resolve()), "Symlink source")
    # Used ONLY in credential-free verification, never in prepare/publish.
    def git(*args):
        return subprocess.check_output(["git", "-C", root, *args], text=True)
    require(git("rev-parse", "HEAD").strip() == proposal["sha"], "Verification checkout SHA mismatch")
    require(git("ls-tree", proposal["sha"], "--", change["path"]).startswith("100644 blob "),
            "Verification source must be a regular non-executable file")
    original = git("show", proposal["sha"] + ":" + change["path"])
    return path, original, git


def publish(api, proposal, proof, run_id, attempt, mapping, slack_ready):
    verify_publication(api, proposal, proof, run_id, attempt)
    # Reread all evidence on the fresh runner. Downloaded proposals grant no authority.
    fresh = prepare(api, proposal["source_run"])
    require(fresh == proposal, "Evidence changed after verification")
    sha, change = fresh["sha"], fresh["change"]
    original = api.source(sha, change["path"])
    fixed = replace_literal(original, change)
    commit = api.get(f"git/commits/{sha}")
    url = f"https://github.com/{api.repo}"
    fix_title = "test(e2e): align CLI help synopsis with observed output"
    source_link = f"[{api.repo} #{fresh['pr']} — {title(fresh['pr_title'])}]({url}/pull/{fresh['pr']})"
    run_link = f"[Swift E2E Tests run {fresh['failure_run']}]({url}/actions/runs/{fresh['failure_run']})"
    slack_id = mapping.get(fresh["author"], "")
    mapped = isinstance(slack_id, str) and re.fullmatch(r"U[A-Z0-9]{8,}", slack_id)
    notice = "Slack author mapping is missing/invalid; author will not be pinged." if not mapped else ""
    if not slack_ready:
        notice = "Slack token/channel not configured; Slack notification will not be attempted. " + notice
    body = (f"{marker(fresh['breaking'])}\n<!-- notification-attempts: reserved; never automatically retry -->\n\n"
            f"E2E repair bot: proposed literal-only correction after {source_link}.\n\n"
            f"Failure: {run_link}. Preceding green: [Swift E2E Tests run {fresh['green_run']}]({url}/actions/runs/{fresh['green_run']}).\n\n"
            f"The recorded `{change['command']} --help` synopsis changed only brackets/spacing. "
            "One positive assertion keeps checking the complete observed synopsis; no assertions or tests are removed. "
            "The exact failed test passed with this literal change in the separate verification job.\n\n"
            "Draft for human inspection. No merge or branch updates. GitHub/Slack notification attempts are reserved "
            "by this draft before posting. Partial/ambiguous failures require manual follow-up; later runs never retry them.\n\n" + notice)
    # No checkout, subprocess, hooks, tests, model or patch execution in this job.
    blob = api.post("git/blobs", {"content": fixed, "encoding": "utf-8"})
    tree = api.post("git/trees", {"base_tree": commit["tree"]["sha"], "tree": [
        {"path": change["path"], "mode": "100644", "type": "blob", "sha": blob["sha"]}]})
    fixed_commit = api.post("git/commits", {"message": fix_title + "\n\nE2E repair bot: literal-only verified synopsis correction.",
                                          "tree": tree["sha"], "parents": [sha]})
    require(api.get("git/ref/heads/main")["object"]["sha"] == sha, "Main advanced before branch creation")
    api.post("git/refs", {"ref": "refs/heads/" + branch(fresh["breaking"]), "sha": fixed_commit["sha"]})
    require(api.get("git/ref/heads/main")["object"]["sha"] == sha, "Main advanced; branch retained for manual follow-up")
    fix = api.post("pulls", {"title": fix_title, "body": body, "head": branch(fresh["breaking"]), "base": "main", "draft": True})
    fix_link = f"[{api.repo} #{fix['number']} — {fix_title}]({url}/pull/{fix['number']})"
    comment = (f"{marker(fresh['breaking'])}\nE2E repair bot: @{fresh['author']}, "
               f"the first failing main merge for this synopsis assertion is {source_link}. "
               f"Failure: {run_link}. Verified draft fix: {fix_link}. Human inspection required.")
    # Creation of the draft durably consumes BOTH automatic notification attempts.
    api.post(f"issues/{fresh['pr']}/comments", {"body": comment})
    if notice:
        print(notice)
    if not slack_ready:
        return None
    ping = f"<@{slack_id}>" if mapped else f"Author {fresh['author']} has no Slack mapping; not pinged."
    return {"text": f"E2E repair bot: {ping}\nFirst failing main merge for CLI synopsis drift:\n"
            f"*{title(fresh['pr_title'])}* {api.repo} #{fresh['pr']}\n{url}/pull/{fresh['pr']}\n\n"
            f"*Swift E2E Tests* run {fresh['failure_run']}\n{url}/actions/runs/{fresh['failure_run']}\n\n"
            f"*{fix_title}* {api.repo} #{fix['number']}\n{url}/pull/{fix['number']}\n\n"
            "Verified literal-only draft. Human inspection required.", "unfurl_links": False, "unfurl_media": False}


def output(key, value):
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            stream.write(f"{key}={value}\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("prepare", "apply", "check", "publish"))
    parser.add_argument("--proposal", default="proposal.json")
    parser.add_argument("--source", default=".")
    parser.add_argument("--results")
    parser.add_argument("--proof", default="verification.json")
    args = parser.parse_args()
    try:
        if args.mode in ("prepare", "publish"):
            api = GitHub(os.environ["GITHUB_REPOSITORY"], os.environ["GH_TOKEN"], os.environ.get("E2E_FIX_BOT_TOKEN"))
        if args.mode == "prepare":
            proposal = prepare(api, int(os.environ["SOURCE_RUN_ID"]))
            Path(args.proposal).write_text(json.dumps(proposal, indent=2) + "\n")
            output("candidate", "true")
        else:
            proposal = json.loads(Path(args.proposal).read_text())
            change = proposal["change"]
            if args.mode == "apply":
                path, original, git = checked_source(proposal, args.source)
                require(not git("diff", "HEAD", "--name-only"), "Verification checkout is dirty")
                path.write_text(replace_literal(original, change))
                output("filter", test_filter(change))
            elif args.mode == "check":
                path, original, git = checked_source(proposal, args.source)
                require(path.read_text() == replace_literal(original, change) and
                        git("diff", "HEAD", "--name-only").splitlines() == [change["path"]] and
                        not git("diff", "HEAD", "--summary"),
                        "Verification source differs from exact candidate")
                verify_results(change, Path(args.results).read_text())
                Path(args.proof).write_text(json.dumps(verification(proposal, int(os.environ["GITHUB_RUN_ID"]),
                                                                  int(os.environ["GITHUB_RUN_ATTEMPT"]))))
            else:
                mapping = json.loads(os.environ.get("E2E_SLACK_AUTHORS", "{}"))
                require(isinstance(mapping, dict), "Invalid author mapping")
                channel = os.environ.get("SLACK_CHANNEL", "")
                slack_ready = os.environ.get("SLACK_READY") == "true" and valid_channel(channel)
                require(proposal["sha"] == os.environ["SOURCE_SHA"] and
                        proposal["source_run"] == int(os.environ["SOURCE_RUN_ID"]), "Proposal does not match triggering run")
                payload = publish(api, proposal, json.loads(Path(args.proof).read_text()), int(os.environ["GITHUB_RUN_ID"]),
                                  int(os.environ["GITHUB_RUN_ATTEMPT"]), mapping, slack_ready)
                if payload:
                    payload["channel"] = channel
                    Path("slack-payload.json").write_text(json.dumps(payload))
                    output("notify", "true")
    except Skip as error:
        print(f"Skipped: {error}")
        if os.environ.get("GITHUB_STEP_SUMMARY"):
            with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
                summary.write(f"E2E synopsis repair skipped: {error}\n")
        if args.mode in ("apply", "check"):
            raise SystemExit(1)
    except (urllib.error.URLError, ValueError, KeyError, ET.ParseError, zipfile.BadZipFile) as error:
        # Never print HTTP bodies, signed artifact URLs or raw untrusted evidence.
        print(f"Repair failed ({type(error).__name__}); inspect stage, manual follow-up required. No automatic write retries.")
        raise SystemExit(1)


if __name__ == "__main__":
    main()
