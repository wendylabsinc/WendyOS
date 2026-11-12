"""Run the release workflow's actual tag step against a local Git-backed API fixture."""
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

WORKFLOW = Path(__file__).resolve().parents[1] / "workflows/build.yml"


def tag_step():
    lines = WORKFLOW.read_text().splitlines()
    start = lines.index("      - name: Tag the built commit")
    run = next(i for i in range(start, len(lines)) if lines[i] == "        run: |")
    end = next(i for i in range(run + 1, len(lines)) if lines[i] and not lines[i].startswith("          "))
    return textwrap.dedent("\n".join(lines[run + 1:end]))


class BuiltCommitTagTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        subprocess.run(["git", "init", "-q", "-b", "main", str(self.repo)], check=True)
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Fixture")
        self.git("commit", "--allow-empty", "-qm", "Built commit")
        self.built = self.git("rev-parse", "HEAD")
        self.git("commit", "--allow-empty", "-qm", "Merged while build ran")
        self.latest = self.git("rev-parse", "HEAD")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        gh = self.bin / "gh"
        gh.write_text("#!/usr/bin/env python3\n" + textwrap.dedent("""\
            import os, subprocess, sys
            args = sys.argv[1:]
            assert args[0] == "api", args
            endpoint = args[1]
            prefix = "repos/fixture/repository/git/"
            assert endpoint.startswith(prefix), endpoint
            if os.environ.get("FAIL_API") == "1":
                sys.exit(1)
            git = ["git", "-C", os.environ["FIXTURE_REPO"]]
            if endpoint.startswith(prefix + "ref/tags/"):
                ref = "refs/tags/" + endpoint.removeprefix(prefix + "ref/tags/")
                sys.exit(subprocess.run(git + ["show-ref", "--verify", "--quiet", ref]).returncode)
            assert endpoint == prefix + "refs", endpoint
            fields = dict(args[i + 1].split("=", 1) for i, arg in enumerate(args) if arg == "-f")
            subprocess.run(git + ["update-ref", fields["ref"], fields["sha"], "0" * 40], check=True)
            """))
        gh.chmod(0o755)

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], text=True).strip()

    def run_step(self, **overrides):
        env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                   GITHUB_REPOSITORY="fixture/repository", GITHUB_SHA=self.built,
                   TAG="2026.11.12", FIXTURE_REPO=str(self.repo), **overrides)
        return subprocess.run(["bash", "-c", tag_step()], env=env, text=True, capture_output=True, check=True)

    def test_tags_built_sha_after_main_advances(self):
        self.run_step()
        self.assertEqual(self.git("rev-parse", "2026.11.12"), self.built)
        self.assertEqual(self.git("rev-parse", "main"), self.latest)
        self.assertNotEqual(self.built, self.latest)

    def test_retry_preserves_existing_tag(self):
        self.run_step()
        self.run_step(GITHUB_REF="refs/heads/main")
        self.assertEqual(self.git("rev-parse", "2026.11.12"), self.built)

    def test_api_failure_reports_unpinned_fallback(self):
        # Preserve the current documented fallback, without claiming it pins a SHA.
        result = self.run_step(FAIL_API="1")
        self.assertIn("::warning::Could not tag", result.stdout)
        self.assertEqual(self.git("tag", "--list"), "")


if __name__ == "__main__":
    unittest.main()
