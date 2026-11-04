"""Issues-only publisher. Receives trusted replay records, never free-form model posts."""

import datetime, hashlib, json, os, pathlib, re, stat, ssl, time, urllib.error, urllib.request
from common import NoRedirect, save, scrub, CANCEL


class GitHub:
    def __init__(self, repo, token_file):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
            raise ValueError("Invalid allowed repository")
        self.repo = repo
        self.token_file = pathlib.Path(token_file)

    def request(self, method, suffix, data=None):
        if CANCEL.is_set():
            raise RuntimeError("Scout is stopping")
        allowed = (
            method == "GET"
            and (suffix.startswith("issues?state=all&") or suffix == "labels/ai-found")
        ) or (method == "POST" and suffix in {"issues", "labels"})
        if not allowed:
            raise ValueError("Publisher route refused")
        fd = os.open(self.token_file, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd) as f:
            st = os.fstat(f.fileno())
            if (
                not stat.S_ISREG(st.st_mode)
                or st.st_uid != 0
                or stat.S_IMODE(st.st_mode) != 0o600
            ):
                raise ValueError("Publisher token must be root-owned mode 0600")
            token = f.read(4096).strip()
        if not re.fullmatch(r"(?:github_pat_|ghs_)[A-Za-z0-9_]+", token):
            raise ValueError("Expected a scoped fine-grained or installation token")
        req = urllib.request.Request(
            "https://api.github.com/repos/" + self.repo + "/" + suffix,
            method=method,
            data=json.dumps(data).encode() if data else None,
            headers={
                "Authorization": "Bearer " + token,
                "User-Agent": "Wendy-Scout",
                "Accept": "application/vnd.github+json",
                "Content-Type": "application/json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        with urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=ssl.create_default_context()),
            NoRedirect,
        ).open(req, timeout=20) as r:
            payload = r.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024:
            raise ValueError("GitHub response exceeds limit")
        return json.loads(payload)


def find_issue(transport, matches):
    """Complete all-state scan using the credentialed publisher transport only."""
    deadline = time.monotonic() + 45
    for page in range(1, 101):
        if CANCEL.is_set() or time.monotonic() > deadline:
            raise ValueError("Duplicate scan deadline; publication held")
        issues = transport.request(
            "GET",
            f"issues?state=all&per_page=100&sort=created&direction=desc&page={page}",
        )
        for issue in issues:
            if "pull_request" not in issue and matches(issue):
                return issue
        if len(issues) < 100:
            return None
    raise ValueError("Duplicate scan exceeded page limit; publication held")


class Publisher:
    def __init__(self, root, repo="wendylabsinc/WendyOS", transport=None):
        self.root = pathlib.Path(root)
        self.transport = transport or GitHub(
            repo, self.root / "secrets/github-issues-token"
        )
        self.repo = repo
        self.repository_key = hashlib.sha256(repo.encode()).hexdigest()
        self.ledger = self.root / ("publisher." + self.repository_key + ".json")

    @staticmethod
    def fingerprint(finding):
        identity = {k: finding.get(k) for k in ["kind", "probe", "field", "agent"]}
        identity["versions"] = [r["version"] for r in finding["results"][:2]]
        identity["failure_identity"] = [
            r.get("failure_identity") for r in finding["results"][1::2]
        ]
        return hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()

    def publish(self, finding):
        if (
            finding.get("already_fixed_in_newer_stable")
            or finding.get("current_stable_unverified")
            or finding.get("verified") is not True
            or len(finding.get("results", [])) != 4
        ):
            raise ValueError("Only trusted twice-replayed findings are publishable")
        fp = self.fingerprint(finding)
        record = (
            json.loads(self.ledger.read_text())
            if self.ledger.exists()
            else {"attempts": [], "published": {}}
        )
        if fp in record["published"]:
            return {
                "status": "duplicate-local",
                "fingerprint": fp,
                "repository_key": self.repository_key,
            }
        versions = [r["version"] for r in finding["results"][:2]]
        failure_ids = [r.get("failure_identity") for r in finding["results"][1::2]]
        marker = "<!-- wendy-scout:" + fp + " -->"
        title = f"{finding['probe']}: {finding['kind']} {finding.get('field', '')} [{fp[:12]}]"
        now = datetime.datetime.now(datetime.timezone.utc)
        today = now.date().isoformat()
        recent = record["attempts"]
        if sum(x["day"] == today for x in recent) >= 2 or any(
            now.timestamp() - x["time"] < 3600 for x in recent
        ):
            return {
                "status": "rate-limited",
                "fingerprint": fp,
                "repository_key": self.repository_key,
            }

        def matches(issue):
            body = issue.get("body") or ""
            return (
                marker in body
                or issue["title"].lower() == title.lower()
                or (
                    failure_ids[0]
                    and str(failure_ids[0]) in body
                    and f'"probe": "{finding["probe"]}"' in body
                    and f'"predicate": "{finding["kind"]}"' in body
                    and f'"field": "{finding.get("field", "")}"' in body
                )
            )

        duplicate = find_issue(self.transport, matches)
        if duplicate:
            return {
                "status": "duplicate",
                "issue": duplicate["html_url"],
                "fingerprint": fp,
                "repository_key": self.repository_key,
            }
        now = datetime.datetime.now(datetime.timezone.utc)
        today = now.date().isoformat()
        recent = record["attempts"]
        if sum(x["day"] == today for x in recent) >= 2 or any(
            now.timestamp() - x["time"] < 3600 for x in recent
        ):
            return {
                "status": "rate-limited",
                "fingerprint": fp,
                "repository_key": self.repository_key,
            }
        try:
            self.transport.request("GET", "labels/ai-found")
        except urllib.error.HTTPError as e:
            if e.code != 404:
                raise
            e.close()
            self.transport.request(
                "POST",
                "labels",
                {
                    "name": "ai-found",
                    "color": "5319e7",
                    "description": "Reproduced finding from Wendy Scout",
                },
            )
        # Structured, scrubbed output only. No user/model-written title/body,
        # comments, mentions, links from source text, or script stdout.
        observations = finding["results"] + finding.get("current_stable", [])
        if finding["probe"].startswith("help_"):
            # Preserve every replay without publishing large CLI help buffers.
            # The flag predicate is the observation; complete text stays local.
            observations = [
                {
                    **r,
                    "output": {
                        "flag": finding.get("field", ""),
                        "present": finding.get("field", "") in str(r["output"]),
                        "text_sha256": hashlib.sha256(
                            str(r["output"]).encode()
                        ).hexdigest(),
                    },
                }
                for r in observations
            ]
        # Output has already passed the typed public projection; do not regex
        # away version fields or replace a real semantic predicate with a mask.
        body = (
            "> **In plain terms:** The same CLI operation behaves differently across the compared releases on an unchanged Agent. Wendy Scout reproduced the difference twice; the root cause is unconfirmed.\n\n"
            + marker
            + "\n\n"
            + "Identity: `"
            + json.dumps(
                {
                    "probe": finding["probe"],
                    "predicate": finding["kind"],
                    "field": finding.get("field", ""),
                },
                sort_keys=True,
            )
            + "`\n\n"
            + "Agent: `"
            + str(finding["agent"])
            + "`. Predicate: `"
            + finding["kind"]
            + "`; field: `"
            + str(finding.get("field", ""))
            + "`.\n\n"
            + "Commands, exact CLI versions, exits and scrubbed observations:\n\n```json\n"
            + json.dumps(observations, indent=2)
            + "\n```\n\n"
            + "This reports the observed compatibility difference. Root cause is unconfirmed. No Agent/OS change or physical capture was performed."
        )
        # Fail closed on credential-shaped output even after structural scrubbing.
        if re.search(
            r"github_pat_|gh[opusr]_|BEGIN .*PRIVATE|https?://(?!github.com/)", body
        ):
            raise ValueError("Potential private/unapproved content; publication held")
        if len(body) > 50000:
            raise ValueError(
                "Replay evidence exceeds issue body limit; publication held"
            )
        recent.append({"day": today, "time": now.timestamp(), "fingerprint": fp})
        record["attempts"] = recent[-20:]
        save(self.ledger, record)
        issue = self.transport.request(
            "POST",
            "issues",
            {"title": title, "body": body, "labels": ["ai-found"]},
        )
        record["published"][fp] = issue["html_url"]
        save(self.ledger, record)
        return {
            "status": "published",
            "issue": issue["html_url"],
            "fingerprint": fp,
            "repository_key": self.repository_key,
        }
