"""Trusted artifact-only publication helpers; never execute a preview checkout."""
import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
from pathlib import Path
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener, urlopen

from versioning import derive, validate


class SafeRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, url):
        if urlsplit(url).scheme != "https":
            raise ValueError("Authenticated downloads must remain on HTTPS")
        redirected = super().redirect_request(request, response, code, message, headers, url)
        if redirected is not None and urlsplit(request.full_url).netloc != urlsplit(url).netloc:
            redirected.remove_header("Authorization")
        return redirected


def authenticated_open(request):
    return build_opener(SafeRedirect()).open(request, timeout=30)


def api(path, data=None):
    request = Request("https://api.github.com/" + path,
                      headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"],
                               "Accept": "application/vnd.github+json"},
                      data=json.dumps(data).encode() if data is not None else None)
    with authenticated_open(request) as response:
        return json.load(response)


def context(repository, run_id, build_attempt=None):
    run = api(f"repos/{repository}/actions/runs/{run_id}")
    if run["conclusion"] != "success" or run["head_repository"]["full_name"] != repository:
        raise ValueError("Only successful same-repository builds may publish")
    if run["path"] not in (".github/workflows/ci.yml", ".github/workflows/release.yml"):
        raise ValueError("Unexpected build workflow")
    source = run["head_sha"]
    content = api(f"repos/{repository}/contents/VERSION?ref={source}")
    base = base64.b64decode(content["content"]).decode().strip()
    branch, pr = run["head_branch"], None
    if run["event"] == "pull_request":
        if len(run["pull_requests"]) != 1:
            raise ValueError("Cannot identify the preview PR")
        pr = run["pull_requests"][0]["number"]
        pull = api(f"repos/{repository}/pulls/{pr}")
        if pull["head"]["repo"]["full_name"] != repository or pull["head"]["sha"] != source:
            raise ValueError("Forked or superseded PR build")
        branch, channel = pull["head"]["ref"], "pr"
    elif run["event"] == "push" and branch == "main":
        if api(f"repos/{repository}/commits/main")["sha"] != source:
            raise ValueError("Superseded nightly build")
        channel = "nightly"
    elif run["event"] == "push" and branch == "v" + base:
        if tag_commit(repository, branch) != source:
            raise ValueError("Stable tag no longer identifies the tested source")
        channel = "stable"
    elif run["event"] == "workflow_dispatch":
        from urllib.parse import quote
        ref = api(f"repos/{repository}/git/ref/heads/{quote(branch, safe='')}")
        if ref["object"]["sha"] != source:
            raise ValueError("Superseded manual preview or non-branch dispatch")
        channel = "pr"
    else:
        raise ValueError("Unsupported publication trigger")
    attempt = run["run_attempt"] if build_attempt is None else build_attempt
    if type(attempt) is not int or not 0 < attempt <= run["run_attempt"]:
        raise ValueError("Invalid originating build attempt")
    return derive(base, channel, source_sha=source, branch=branch, pr=pr,
                  run_id=run["id"], attempt=attempt)


def tag_commit(repository, tag):
    target = api(f"repos/{repository}/git/ref/tags/{tag}")["object"]
    while target["type"] == "tag":
        target = api(f"repos/{repository}/git/tags/{target['sha']}")["object"]
    if target["type"] != "commit":
        raise ValueError("Release tag must identify a commit")
    return target["sha"]


def verify(directory, expected):
    validate(expected)
    release = json.loads((directory / "release.json").read_text())
    if release["identity"] != expected or release["version"] != expected["version"]:
        raise ValueError("Artifact identity differs from the authorized workflow run")
    entries = set()
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        checksum, name = line.split("  ", 1)
        path = Path(name)
        if not re.fullmatch(r"[0-9a-f]{64}", checksum) or path.is_absolute() or ".." in path.parts:
            raise ValueError("Invalid checksum entry")
        if name in entries or name == "SHA256SUMS":
            raise ValueError("Duplicate or self-referencing checksum")
        entries.add(name)
        file = directory / path
        if file.resolve().parent != directory.resolve() or file.is_symlink():
            raise ValueError("Release assets must be ordinary top-level files")
        if hashlib.sha256(file.read_bytes()).hexdigest() != checksum:
            raise ValueError("Release asset checksum mismatch: " + name)
    actual = {path.name for path in directory.iterdir() if path.name != "SHA256SUMS"}
    if actual != entries:
        raise ValueError("Unsealed release assets")
    verify_versions(directory, expected)
    return release


def verify_versions(directory, expected):
    import tarfile
    import zipfile
    from email import message_from_bytes
    from build import TARGETS
    package = directory / f"wendylabs-core-{expected['version']}.tgz"
    with tarfile.open(package) as archive:
        metadata = json.load(archive.extractfile("package/package.json"))
    if metadata["name"] != "@wendylabs/core" or metadata["version"] != expected["version"]:
        raise ValueError("npm payload has a different package version")
    wheels = list(directory.glob("*.whl"))
    if len(wheels) != len(TARGETS):
        raise ValueError("Missing Python wheel targets")
    for wheel in wheels:
        with zipfile.ZipFile(wheel) as archive:
            names = [name for name in archive.namelist() if name.endswith(".dist-info/METADATA")]
            if len(names) != 1:
                raise ValueError("Ambiguous wheel metadata")
            metadata = message_from_bytes(archive.read(names[0]))
            if metadata["Name"] != "wendy-core" or metadata["Version"] != expected["pythonVersion"]:
                raise ValueError("Wheel payload has a different package version")
    for suffix in TARGETS:
        file = directory / f"wendy-core-{expected['version']}-{suffix}.tar.gz"
        with tarfile.open(file) as archive:
            if archive.extractfile("VERSION").read().decode().strip() != expected["version"]:
                raise ValueError("Native archive has a different package version")
    from swift_release import asset
    with zipfile.ZipFile(directory / asset("CWendyCoreLinux", expected["version"])) as archive:
        info = json.loads(archive.read("CWendyCore.artifactbundle/info.json"))
        if info["artifacts"]["CWendyCore"]["version"] != expected["version"]:
            raise ValueError("Swift artifact bundle has a different package version")
    with zipfile.ZipFile(directory / asset("CWendyCoreApple", expected["version"])) as archive:
        archive.getinfo("CWendyCore.xcframework/Info.plist")


def go_tag(repository, expected):
    from urllib.error import HTTPError
    validate(expected)
    tag = "v" + expected["version"]
    try:
        source = tag_commit(repository, tag)
    except HTTPError as error:
        if error.code != 404 or expected["channel"] == "stable":
            raise
        api(f"repos/{repository}/git/refs", {"ref": "refs/tags/" + tag, "sha": expected["sourceSha"]})
        return
    if source != expected["sourceSha"]:
        raise ValueError("An existing module version cannot be moved")


def swift_tag(directory, repository, expected):
    """Create or verify the bare Swift tag after the GitHub release assets exist."""
    from urllib.error import HTTPError
    from swift_release import release_manifest
    validate(expected)
    tag, source = expected["version"], expected["sourceSha"]

    def package_swift(ref):
        return base64.b64decode(api(f"repos/{repository}/contents/Package.swift?ref={ref}")["content"]).decode()

    text = release_manifest(package_swift(source), repository, tag, directory)
    try:
        commit = tag_commit(repository, tag)
    except HTTPError as error:
        if error.code != 404:
            raise
        tree = api(f"repos/{repository}/git/commits/{source}")["tree"]["sha"]
        blob = api(f"repos/{repository}/git/blobs", {"content": text, "encoding": "utf-8"})["sha"]
        tree = api(f"repos/{repository}/git/trees", {"base_tree": tree, "tree": [
            {"path": "Package.swift", "mode": "100644", "type": "blob", "sha": blob}]})["sha"]
        commit = api(f"repos/{repository}/git/commits", {
            "message": f"chore: distribute Wendy Core {tag} to SwiftPM", "tree": tree, "parents": [source]})["sha"]
        api(f"repos/{repository}/git/refs", {"ref": "refs/tags/" + tag, "sha": commit})
        return
    parents = [parent["sha"] for parent in api(f"repos/{repository}/git/commits/{commit}")["parents"]]
    if parents != [source] or package_swift(commit) != text:
        raise ValueError("An existing SwiftPM version cannot be moved")


def maven(directory, repository, expected):
    import xml.etree.ElementTree as ET
    value = expected["version"]
    prefix = directory / ("wendy-core-" + value)
    pom = Path(str(prefix) + ".pom")
    root = ET.parse(pom).getroot()
    namespace = {"m": "http://maven.apache.org/POM/4.0.0"}
    for name, wanted in (("groupId", "dev.wendy"), ("artifactId", "wendy-core"), ("version", value)):
        if root.findtext("m:" + name, namespaces=namespace) != wanted:
            raise ValueError("Unexpected Maven coordinates")
    from urllib.error import HTTPError
    from urllib.parse import quote
    credentials = base64.b64encode((os.environ["MAVEN_USERNAME"] + ":" + os.environ["MAVEN_PASSWORD"]).encode()).decode()
    present = 0
    for suffix in (".pom", ".jar", "-sources.jar", "-javadoc.jar"):
        file = Path(str(prefix) + suffix)
        url = f"https://maven.pkg.github.com/{repository}/dev/wendy/wendy-core/{quote(value, safe='')}/{file.name}"
        try:
            with authenticated_open(Request(url, headers={"Authorization": "Basic " + credentials})) as response:
                checksum = hashlib.sha256()
                while chunk := response.read(1024 * 1024):
                    checksum.update(chunk)
            if checksum.hexdigest() != hashlib.sha256(file.read_bytes()).hexdigest():
                raise ValueError("Published Maven version has different bytes")
            present += 1
        except HTTPError as error:
            if error.code != 404:
                raise
    if present == 4:
        return
    subprocess.run(["mvn", "--batch-mode", "org.apache.maven.plugins:maven-deploy-plugin:3.1.4:deploy-file",
                    "-DrepositoryId=github", f"-Durl=https://maven.pkg.github.com/{repository}",
                    f"-DpomFile={pom}", f"-Dfile={prefix}.jar", f"-Dsources={prefix}-sources.jar",
                    f"-Djavadoc={prefix}-javadoc.jar", "-DgeneratePom=false"], check=True)


def registry_check(directory, expected, registry):
    from urllib.error import HTTPError
    from urllib.parse import quote
    if registry == "npm":
        url = "https://registry.npmjs.org/@wendylabs%2fcore/" + quote(expected["version"], safe="")
    else:
        url = "https://pypi.org/pypi/wendy-core/" + quote(expected["pythonVersion"], safe="") + "/json"
    try:
        with urlopen(url, timeout=30) as response:
            remote = json.load(response)
    except HTTPError as error:
        if error.code != 404:
            raise
        remote = None
    if registry == "npm":
        if remote is not None:
            package = directory / f"wendylabs-core-{expected['version']}.tgz"
            integrity = "sha512-" + base64.b64encode(hashlib.sha512(package.read_bytes()).digest()).decode()
            if remote["dist"]["integrity"] != integrity:
                raise ValueError("Published npm version has different bytes")
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"exists={'true' if remote is not None else 'false'}\n")
    elif remote is not None:
        for asset in remote["urls"]:
            file = directory / asset["filename"]
            if not file.is_file() or hashlib.sha256(file.read_bytes()).hexdigest() != asset["digests"]["sha256"]:
                raise ValueError("Published Python version has different bytes")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("context", "verify", "go-tag", "swift-tag", "maven", "npm-check", "pypi-check"))
    parser.add_argument("--run-id", type=int)
    parser.add_argument("--directory", type=Path, default=Path("release"))
    args = parser.parse_args()
    if args.command != "verify":
        raise ValueError("Core publication is disabled pending the monorepo release design")
    repository = os.environ["GITHUB_REPOSITORY"]
    if args.command == "context":
        supplied = json.loads((args.directory / "release.json").read_text())
        expected = context(repository, args.run_id, supplied["identity"]["attempt"])
        verify(args.directory, expected)
        text = json.dumps(expected, separators=(",", ":"))
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"identity={text}\nversion={expected['version']}\nchannel={expected['channel']}\n")
        print(text)
    else:
        expected = validate(json.loads(os.environ["CORE_BUILD_IDENTITY"]))
        if args.command != "verify" and expected["channel"] == "dev":
            raise ValueError("Local development versions cannot be published")
        verify(args.directory, expected)
        if args.command == "go-tag":
            go_tag(repository, expected)
        elif args.command == "swift-tag":
            swift_tag(args.directory, repository, expected)
        elif args.command == "maven":
            maven(args.directory, repository, expected)
        elif args.command in ("npm-check", "pypi-check"):
            registry_check(args.directory, expected, args.command.split("-")[0])


if __name__ == "__main__":
    main()
