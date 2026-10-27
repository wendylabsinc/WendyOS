"""SwiftPM release manifests and an installed-consumer check.

The local smoke check creates a temporary repository-root package to validate
packed binaries. It does not make the nested WendyOS package remotely consumable.
Remote Swift distribution is unresolved and the publication CLI is disabled.
The release_manifest helper retains the standalone repository format for legacy
unit tests; no WendyOS workflow uses it to publish tags or release assets.
"""
import argparse
import re
import shutil
import tempfile
from pathlib import Path

from build import ROOT, check_version, copy, digest, run
from versioning import RELEASE, STABLE

BEGIN = "// BEGIN BINARY TARGETS\n"
END = "// END BINARY TARGETS\n"
ASSETS = {"CWendyCoreApple": "xcframework", "CWendyCoreLinux": "artifactbundle"}
VERSION = RELEASE + r"|" + STABLE + r"-(?:dev(?:\+[0-9a-f]{7})?|nightly\.[1-9][0-9]*\.[1-9][0-9]*|pr\.(?:[1-9][0-9]*|manual)-[a-z0-9-]+\.[1-9][0-9]*\.[1-9][0-9]*)"


def asset(name, value):
    return f"CWendyCore-{value}.{ASSETS[name]}.zip"


def manifest(text, sources):
    """Replace the local binary-target block with the given `.binaryTarget` sources."""
    if text.count(BEGIN) != 1 or text.count(END) != 1 or text.index(BEGIN) > text.index(END):
        raise ValueError("Package.swift must contain one binary-target block")
    lines = ["let binaryTargets: [Target] = ["]
    lines += [f'    .binaryTarget(name: "{name}", {sources[name]}),' for name in ASSETS]
    lines += ["]"]
    return text[:text.index(BEGIN) + len(BEGIN)] + "\n".join(lines) + "\n" + text[text.index(END):]


def release_manifest(text, repository, value, directory):
    if not re.fullmatch(VERSION, value) or not re.fullmatch(r"[\w.-]+/[\w.-]+", repository):
        raise ValueError("Invalid Swift release version or repository")
    base = f"https://github.com/{repository}/releases/download/v{value}/"
    return manifest(text, {
        name: f'url: "{base}{asset(name, value)}", checksum: "{digest(directory / asset(name, value))}"'
        for name in ASSETS
    })


def smoke(directory, xcode_mcp=False):
    """Resolve the packed binaries through a real tagged SwiftPM dependency."""
    value = check_version()
    with tempfile.TemporaryDirectory(prefix="wendy-core-swift-") as temp:
        package = Path(temp) / "wendy-core"
        for name in ("Sources", "Tests"):
            shutil.copytree(ROOT / "distributions/swift" / name, package / "distributions/swift" / name)
        for name in ASSETS:
            copy(directory / asset(name, value), package / "binaries" / asset(name, value))
        # Local zip paths stand in for the release URLs; the archives are the same.
        text = manifest((ROOT / "Package.swift").read_text(),
                        {name: f'path: "binaries/{asset(name, value)}"' for name in ASSETS})
        (package / "Package.swift").write_text(text)
        identity = ["-c", "user.name=Wendy Core", "-c", "user.email=core@wendy.invalid"]
        run("git", "init", "-q", "-b", "main", package)
        run("git", "-C", package, "add", "-A")
        run("git", *identity, "-C", package, "commit", "-q", "-m", "Release-only Swift manifest")
        run("git", "-C", package, "tag", value)
        consumer = Path(temp) / "consumer"
        tests = consumer / "Tests/SmokeTests"
        tests.mkdir(parents=True)
        (consumer / "Package.swift").write_text('''// swift-tools-version: 6.2
import PackageDescription
let package = Package(
    name: "CoreConsumer",
    platforms: [.macOS(.v15), .iOS(.v18)],
    dependencies: [.package(url: "%s", exact: "%s")],
    targets: [.testTarget(name: "SmokeTests", dependencies: [.product(name: "WendyCore", package: "wendy-core")])]
)
''' % (package.as_uri(), value))
        (tests / "Smoke.swift").write_text('''import Testing
import WendyCore
@Test func consumesCoreThroughAnOrdinaryDependency() {
    #expect(WendyHelloWorld() == 42)
    #expect(WendyCoreVersion() == "%s")
}
''' % value)
        if xcode_mcp:
            run("xcodebuildmcp", "swift-package", "test", "--package-path", consumer)
        else:
            run("swift", "test", cwd=consumer)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, help="Directory containing the packed release assets")
    parser.add_argument("--xcode-mcp", action="store_true")
    arguments = parser.parse_args()
    smoke(arguments.directory.resolve(), arguments.xcode_mcp)
