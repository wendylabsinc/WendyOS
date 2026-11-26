#!/usr/bin/env python3
"""Execute the README installer with a real candidate CLI and private release HTTP.

Only the documented installer destination changes: bash receives -d for an owned
directory. A strict curl adapter translates three public URLs to loopback. No
package manager, privilege tool, public download, VM or device is invoked.
"""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import sys
import tarfile
import tempfile
import threading

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
INSTALL_URL = "https://install.wendy.dev/cli.sh"
MANIFEST_URL = "https://install.wendy.dev/manifest.json"
VERSION = "2099.01.01-readme-fixture"


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(path):
    result = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def snippet(text):
    blocks = re.findall(r"```sh\n(.*?)\n```", text, re.S)
    matches = [block.splitlines() for block in blocks if INSTALL_URL in block]
    require(len(matches) == 1, "README_INSTALL_BLOCK: expected one installer block")
    lines = matches[0]
    require(lines == [f"curl -fsSL {INSTALL_URL} | bash", "wendy --version"],
            "README_INSTALL_BLOCK: review changed installer commands before executing")
    return lines


def adapter(argv):
    """The fixture PATH's curl; real curl still performs every HTTP transfer."""
    plan = json.loads(Path(os.environ["README_INSTALL_MAP"]).read_text())
    require(len(argv) in (2, 4) and argv[0] == "-fsSL",
            "unexpected installer curl arguments")
    if len(argv) == 4:
        require(argv[1] == "-o", "unexpected curl destination option")
        destination = Path(argv[2]).resolve()
        require(destination.is_relative_to(Path(plan["work"])), "download outside owned work")
    url = argv[-1]
    require(url in plan["urls"], "unapproved installer URL: " + url)
    translated = plan["origin"] + plan["urls"][url]
    # A redirect is an error, including redirects to another loopback route.
    command = [plan["curl"], "--noproxy", "*", "--proto", "=http", "--max-time", "15",
               "--fail", "--silent", "--show-error", "--location", "--max-redirs", "0"]
    if len(argv) == 4:
        command += ["-o", str(destination)]
    command += [translated]
    os.execv(command[0], command)


def run(cli, evidence):
    require(platform.system() in ("Linux", "Darwin"), "Unix installer fixture only")
    cli = cli.resolve(strict=True)
    require(os.access(cli, os.X_OK), "candidate CLI must be executable")
    lines = snippet((ROOT / "README.md").read_text())
    installer = (ROOT / "go/internal/cli/assets/docs/cli.sh").read_bytes()
    evidence.mkdir(parents=True, exist_ok=False)
    spec = importlib.util.spec_from_file_location("owned", HERE / "run-owned-media-test.py")
    owned = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(owned)
    os_name = {"Darwin": "darwin", "Linux": "linux"}[platform.system()]
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[platform.machine()]
    archive_name = f"wendy-cli-{os_name}-{arch}-{VERSION}.tar.gz"
    release_url = f"https://github.com/wendylabsinc/wendy-agent/releases/download/{VERSION}/{archive_name}"
    results = []
    with tempfile.TemporaryDirectory(prefix="wendy-readme-install-") as temporary:
        work = Path(temporary).resolve()
        archive = work / archive_name
        with tarfile.open(archive, "w:gz") as output:
            output.add(cli, arcname=f"wendy-cli-{os_name}-{arch}/wendy", recursive=False)
        for variant in ("normal", "missing-install", "missing-mcp"):
            case = work / variant
            case.mkdir()
            home, tools, destination, downloads = [case / name for name in ("home", "tools", "installed", "tmp")]
            for directory in (home, tools, destination, downloads):
                directory.mkdir()
            (home / ".claude.json").write_text('{}\n')
            # Restrict discovery itself, not just sudo: even root cannot select
            # apt/brew/dnf or invoke any host package manager through this PATH.
            for name in ("bash", "uname", "id", "cat", "mkdir", "install",
                         "rm", "tar", "gzip", "sed", "grep", "head", "tr", "cut", "readlink", "dirname"):
                target = shutil.which(name)
                require(target, "required host tool missing: " + name)
                (tools / name).symlink_to(target)
            # macOS mktemp -d uses its system default even with TMPDIR set.
            # Retain the real utility, but supply an explicit owned template.
            (tools / "mktemp").write_text("#!" + shutil.which("bash") + "\n" +
                '[ "$#" = 1 ] && [ "$1" = -d ] || exit 91\nexec ' +
                shlex.quote(shutil.which("mktemp")) + ' -d "$TMPDIR/installer.XXXXXXXX"\n')
            (tools / "mktemp").chmod(0o755)
            curl = shutil.which("curl")
            require(curl, "curl is required")
            (tools / "curl").write_text("#!" + shutil.which("bash") + "\nexec " +
                                         shlex.quote(sys.executable) + " " + shlex.quote(str(Path(__file__).resolve())) +
                                         ' --curl-adapter "$@"\n')
            (tools / "curl").chmod(0o755)
            served = installer
            if variant == "missing-install":
                needle = b'    install_binary "${TMPDIR_DL}/wendy-cli-'
                require(served.count(needle) == 2, "installer control source changed")
                served = b"\n".join(b"    : # omitted binary installation" if line.startswith(needle) else line
                                     for line in served.split(b"\n"))
            if variant == "missing-mcp":
                require(served.count(b"\nonboard\n") == 1, "onboarding control source changed")
                served = served.replace(b"\nonboard\n", b"\n: # omitted MCP onboarding\n")
            requests = []

            class Handler(BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass

                def do_GET(self):
                    requests.append(self.path)
                    self.connection.settimeout(15)
                    if self.path == "/cli.sh":
                        content = served
                    elif self.path == "/manifest.json":
                        content = json.dumps({"latest": VERSION}).encode()
                    elif self.path == "/" + archive_name:
                        self.send_response(200)
                        self.send_header("Content-Length", str(archive.stat().st_size))
                        self.end_headers()
                        with archive.open("rb") as stream:
                            shutil.copyfileobj(stream, self.wfile)
                        return
                    else:
                        self.send_error(404)
                        return
                    self.send_response(200)
                    self.send_header("Content-Length", str(len(content)))
                    self.end_headers()
                    self.wfile.write(content)

            server = HTTPServer(("127.0.0.1", 0), Handler)
            thread = threading.Thread(target=server.serve_forever, name="readme-installer-http")
            thread.start()
            mapping = case / "map.json"
            mapping.write_text(json.dumps({"work": str(work), "curl": curl,
                "origin": f"http://127.0.0.1:{server.server_port}", "urls": {
                    INSTALL_URL: "/cli.sh", MANIFEST_URL: "/manifest.json", release_url: "/" + archive_name}}))
            environment = {"PATH": str(destination) + os.pathsep + str(tools), "HOME": str(home),
                           "TMPDIR": str(downloads), "WENDY_CONFIG_DIR": str(home / ".wendy"),
                           "WENDY_SECRET_STORE": "file", "WENDY_ANALYTICS": "false",
                           "WENDY_NO_UPDATE_CHECK": "1", "CI": "1", "NO_COLOR": "1",
                           "README_INSTALL_MAP": str(mapping)}
            command = lines[0] + " -s -- -d " + shlex.quote(str(destination)) + "\n" + lines[1]
            log = evidence / (variant + ".log")
            try:
                with log.open("wb") as output:
                    status = owned.run([str(tools / "bash"), "-euo", "pipefail", "-c", command],
                                       60, cwd=str(case), env=environment, stdout=output)
            finally:
                server.shutdown()
                thread.join(20)
                server.server_close()
                require(not thread.is_alive(), "fixture HTTP thread failed to drain")
            installed = destination / "wendy"
            installed_ok = installed.is_file() and digest(installed) == digest(cli)
            config = json.loads((home / ".claude.json").read_text())
            mcp = config.get("mcpServers", {}).get("wendy", {})
            mcp_ok = mcp.get("command") == str(installed) and mcp.get("args") == ["mcp", "serve"]
            require(requests == ["/cli.sh", "/manifest.json", "/" + archive_name],
                    f"unexpected installer HTTP requests: {requests}")
            if variant == "normal":
                require(status == 0 and installed_ok, "README_INSTALLED_CLI: exact candidate did not run")
                require(mcp_ok, "README_MCP_ONBOARDING: installed CLI was not configured")
                versions = re.findall(r"(?m)^wendy version (.+)$", log.read_text())
                require(len(versions) == 2 and versions[0] == versions[1],
                        "README_VERSION: installer and documented version command disagree")
                require("Warning:" not in log.read_text(), "installer emitted a warning")
            elif variant == "missing-install":
                require(status == 127 and not installed_ok and "wendy: command not found" in log.read_text(),
                        "missing-install control did not fail at Wendy invocation")
            else:
                require(status == 0 and installed_ok and not mcp_ok,
                        "missing-mcp control did not reach onboarding assertion")
            results.append({"variant": variant, "exit": status, "installed_sha_matches": installed_ok,
                            "mcp_points_to_installed_cli": mcp_ok, "http_requests": requests,
                            "served_installer_sha256": hashlib.sha256(served).hexdigest()})
    result = {"status": "passed", "cli_sha256": digest(cli), "readme_sha256": digest(ROOT / "README.md"),
              "installer_sha256": hashlib.sha256(installer).hexdigest(), "cases": results,
              "scope": "Standalone Unix install, actual candidate version and MCP setup; public hosting, package managers, winget, templates and device deployment excluded"}
    (evidence / "evidence.json").write_text(json.dumps(result, indent=2) + "\n")
    print("README_INSTALL_EXECUTION_VERIFIED")


if __name__ == "__main__":
    if sys.argv[1:2] == ["--curl-adapter"]:
        adapter(sys.argv[2:])
    else:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("--cli", type=Path, required=True)
        parser.add_argument("--evidence", type=Path, required=True)
        arguments = parser.parse_args()
        run(arguments.cli, arguments.evidence)
