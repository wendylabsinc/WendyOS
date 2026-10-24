"""Local and CI packaging mechanics. No registry access or product behavior."""
import argparse
import hashlib
import io
import json
import os
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
import zipfile
from pathlib import Path

from versioning import TAG_PREFIX, base_version, compatibility_version, identity, python_release_version

ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / ".build"
TARGETS = ("linux-x64", "linux-arm64", "darwin-x64", "darwin-arm64", "win32-x64")
# Swift links Go statically. Each target contributes these files; darwin-arm64
# builds the universal Apple XCFramework (macOS, iOS and iOS Simulator).
SWIFT_FILES = {"linux-x64": ("libCWendyCore.a",), "linux-arm64": ("libCWendyCore.a",),
               "darwin-arm64": ("CWendyCore.xcframework.zip",)}
SWIFT_TRIPLES = {"linux-x64": "x86_64-unknown-linux-gnu", "linux-arm64": "aarch64-unknown-linux-gnu"}
# Name: (GOOS, GOARCH, Xcode SDK, Clang target). Minimums match Package.swift.
APPLE_SLICES = {
    "macos-arm64": ("darwin", "arm64", "macosx", "arm64-apple-macos15.0"),
    "macos-x64": ("darwin", "amd64", "macosx", "x86_64-apple-macos15.0"),
    "ios-arm64": ("ios", "arm64", "iphoneos", "arm64-apple-ios18.0"),
    "ios-simulator-arm64": ("ios", "arm64", "iphonesimulator", "arm64-apple-ios18.0-simulator"),
    "ios-simulator-x64": ("ios", "amd64", "iphonesimulator", "x86_64-apple-ios18.0-simulator"),
}
LIBRARIES = {"linux": "libWendyCore.so", "darwin": "libWendyCore.dylib", "win32": "WendyCore.dll"}
JNI = {"linux": "libwendy_core_jni.so", "darwin": "libwendy_core_jni.dylib", "win32": "wendy_core_jni.dll"}
JS = ROOT / "distributions/javascript"
SWIFT = ROOT / "distributions/swift"
SWIFT_ARTIFACTS = SWIFT / "artifacts"
KOTLIN = ROOT / "distributions/kotlin"
PYTHON = ROOT / "distributions/python"


def run(*args, cwd=ROOT, env=None, input=None):
    environment = {**(os.environ if env is None else env), "WENDY_CORE_VERSION": version()}
    subprocess.run([str(x) for x in args], cwd=cwd, env=environment, input=input, check=True)


def version():
    return identity()["version"]


def go_ldflags(extra=""):
    return f"-X github.com/wendylabsinc/WendyOS/go/wendy-core/core.buildVersion={version()} {extra}".strip()


def check_version(tag=None):
    # Checked-in metadata names the exact release (Python uses its PEP 440 mapping). Package staging applies
    # the effective version without leaving generated edits in the source tree.
    value = base_version()
    go_versions = re.findall(r'^const developmentVersion = "([^"\n]+)"$',
                             (ROOT / "core/version.go").read_text(), re.MULTILINE)
    if go_versions != [compatibility_version(value) + "-dev"]:
        raise ValueError("Go development version differs from the numeric VERSION base + '-dev'")
    package = json.loads((JS / "package.json").read_text())
    if package["version"] != value:
        raise ValueError("npm version differs from VERSION")
    python_versions = re.findall(r'^version\s*=\s*"([^"\n]+)"\s*$', (PYTHON / "pyproject.toml").read_text(), re.MULTILINE)
    if python_versions != [python_release_version(value)]:
        raise ValueError("Python version differs from the PEP 440 mapping of VERSION")
    if f"coreVersion={value}\n" != (KOTLIN / "gradle.properties").read_text():
        raise ValueError("Kotlin version differs from VERSION")
    if tag and (identity()["channel"] != "stable" or tag != TAG_PREFIX + value):
        raise ValueError(f"Stable tag {tag!r} must match {TAG_PREFIX}{value}")
    return version()


def host_target():
    system = {"Linux": "linux", "Darwin": "darwin", "Windows": "win32"}[platform.system()]
    architecture = {"x86_64": "x64", "AMD64": "x64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    target = system + "-" + architecture
    if target not in TARGETS:
        raise ValueError(f"Unsupported initial target: {target}")
    return target


def copy(source, destination):
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, destination)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def go_license(goroot):
    # Homebrew keeps notices beside libexec rather than within GOROOT.
    for path in (goroot / "LICENSE", goroot.parent / "LICENSE"):
        if path.is_file():
            return path
    raise FileNotFoundError("Cannot locate the bundled Go toolchain license")


def stage_licenses():
    for distribution in (JS, PYTHON):
        for name in ("LICENSE", "NOTICE"):
            copy(ROOT / name, distribution / name)


def stage_native(target, directory):
    stage_licenses()
    system = target.split("-")[0]
    library = directory / LIBRARIES[system]
    copy(library, JS / "assets/native" / target / library.name)
    license_file = directory / "LICENSE-Go.txt"
    if license_file.exists():
        copy(license_file, JS / "assets/native" / target / license_file.name)
        copy(license_file, KOTLIN / "src/main/resources/native" / target / license_file.name)
    python_native = PYTHON / "wendy_core/native"
    shutil.rmtree(python_native, ignore_errors=True)
    copy(library, python_native / library.name)
    if license_file.exists():
        copy(license_file, python_native / license_file.name)
    copy(library, KOTLIN / "src/main/resources/native" / target / library.name)
    for name in ("wendy_core.node", JNI[system]):
        path = directory / name
        if path.exists():
            destination = JS / "assets/native" / target / name if name.endswith(".node") else KOTLIN / "src/main/resources/native" / target / name
            copy(path, destination)


def native():
    check_version()
    target = host_target()
    system = target.split("-")[0]
    directory = BUILD / "native" / target
    directory.mkdir(parents=True, exist_ok=True)
    library = directory / LIBRARIES[system]
    env = {**os.environ, "CGO_ENABLED": "1"}
    if system == "darwin":
        env["MACOSX_DEPLOYMENT_TARGET"] = "15.0"
        env["CGO_CFLAGS"] = env.get("CGO_CFLAGS", "-O2 -g") + " -mmacosx-version-min=15.0"
        env["CGO_LDFLAGS"] = env.get("CGO_LDFLAGS", "") + " -mmacosx-version-min=15.0"
    linker = ["-ldflags=" + go_ldflags("-extldflags=-Wl,-install_name,@rpath/libWendyCore.dylib" if system == "darwin" else "")]
    run("go", "build", "-trimpath", "-buildmode=c-shared", *linker, "-o", library, "./cmd/wendy-core-native", env=env)
    # Test an independently compiled C consumer of the public header and symbol.
    compiler = os.environ.get("CC", "cc" if system != "win32" else "gcc")
    executable = directory / ("hello.exe" if system == "win32" else "hello")
    flags = [] if system == "win32" else [f"-Wl,-rpath,{directory}"]
    run(compiler, "-std=c11", "-Iinclude", "examples/c/hello.c", library, *flags, "-o", executable)
    run(executable)
    host_test = directory / ("host_registration.exe" if system == "win32" else "host_registration")
    run(compiler, "-std=c11", "-Iinclude", "tests/c/host_registration.c", library, *flags, "-o", host_test)
    run(host_test)
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    copy(go_license(goroot), directory / "LICENSE-Go.txt")
    stage_native(target, directory)


def swift_headers(directory):
    # Both canonical boundaries are available to native Swift hosts.
    for name in ("WendyCore.h", "WendyPlatform.h"):
        copy(ROOT / "include/WendyCore" / name, directory / name)
    (directory / "module.modulemap").write_text('module CWendyCore {\n    header "WendyCore.h"\n    header "WendyPlatform.h"\n    export *\n}\n')


def swift_notices(directory, go_license_file):
    for name in ("LICENSE", "NOTICE"):
        copy(ROOT / name, directory / name)
    copy(go_license_file, directory / "LICENSE-Go.txt")


def go_archive(output, env):
    output.parent.mkdir(parents=True, exist_ok=True)
    run("go", "build", "-trimpath", "-buildmode=c-archive", "-ldflags=" + go_ldflags(), "-o", output, "./cmd/wendy-core-native", env=env)
    output.with_suffix(".h").unlink()  # cgo's generated header; WendyCore.h is canonical


def zip_directory(directory, destination):
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.unlink(missing_ok=True)
    with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(directory.rglob("*")):
            archive.write(path, path.relative_to(directory.parent).as_posix())


def artifact_bundle(libraries, destination, go_license_file):
    """Write a SwiftPM static-library artifact bundle (SE-0482) for Linux."""
    shutil.rmtree(destination, ignore_errors=True)
    swift_headers(destination / "include")
    swift_notices(destination, go_license_file)
    variants = []
    for target, library in sorted(libraries.items()):
        triple = SWIFT_TRIPLES[target]
        copy(library, destination / triple / "libCWendyCore.a")
        variants.append({"path": f"{triple}/libCWendyCore.a", "supportedTriples": [triple],
                         "staticLibraryMetadata": {"headerPaths": ["include"], "moduleMapPath": "include/module.modulemap"}})
    info = {"schemaVersion": "1.0", "artifacts": {"CWendyCore": {"type": "staticLibrary", "version": version(), "variants": variants}}}
    (destination / "info.json").write_text(json.dumps(info, indent=2) + "\n")


def apple_environment():
    # Each Apple build names its own SDK and minimum; ignore ambient ones such
    # as the SDKROOT that Xcode's python3 shim exports.
    return {name: value for name, value in os.environ.items()
            if name != "SDKROOT" and not name.endswith("_DEPLOYMENT_TARGET")}


def swift():
    """Build the host's Swift binaries and install them for the root Package.swift."""
    check_version()
    target = host_target()
    directory = BUILD / "native" / target
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    go_license_file = go_license(goroot)
    if target.startswith("linux-"):
        library = directory / "libCWendyCore.a"
        go_archive(library, {**os.environ, "CGO_ENABLED": "1"})
        artifact_bundle({target: library}, SWIFT_ARTIFACTS / "CWendyCore.artifactbundle", go_license_file)
    elif target.startswith("darwin-"):
        slices = BUILD / "swift/apple"
        shutil.rmtree(slices, ignore_errors=True)
        env = apple_environment()
        for name, (goos, goarch, sdk, clang_target) in APPLE_SLICES.items():
            sysroot = subprocess.check_output(["xcrun", "--sdk", sdk, "--show-sdk-path"], text=True).strip()
            clang = subprocess.check_output(["xcrun", "--sdk", sdk, "-f", "clang"], text=True).strip()
            flags = f"-isysroot {sysroot} -target {clang_target}"
            go_archive(slices / name / "libCWendyCore.a", {**env, "GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "1",
                                                          "CC": clang, "CGO_CFLAGS": "-O2 " + flags, "CGO_LDFLAGS": flags})
        headers = slices / "Headers/CWendyCore"  # Subdirectory avoids module map clashes in Xcode
        swift_headers(headers)
        arguments = []
        for family in ("macos", "ios", "ios-simulator"):
            members = [slices / name / "libCWendyCore.a" for name in APPLE_SLICES if name.rsplit("-", 1)[0] == family]
            library = slices / family / "libCWendyCore.a"
            library.parent.mkdir(parents=True, exist_ok=True)
            run("lipo", "-create", *members, "-output", library)
            arguments += ["-library", library, "-headers", headers.parent]
        framework = SWIFT_ARTIFACTS / "CWendyCore.xcframework"
        shutil.rmtree(framework, ignore_errors=True)
        run("xcodebuild", "-create-xcframework", *arguments, "-output", framework)
        swift_notices(framework, go_license_file)
        zip_directory(framework, directory / "CWendyCore.xcframework.zip")
    else:
        raise ValueError(f"Swift is not supported on {target}")


def node_addon():
    target = host_target()
    npm = "npm.cmd" if platform.system() == "Windows" else "npm"
    run(npm, "run", "build:native", cwd=JS)
    output = JS / "native/build/Release/wendy_core.node"
    copy(output, BUILD / "native" / target / output.name)
    stage_native(target, BUILD / "native" / target)


def jni():
    target = host_target()
    system = target.split("-")[0]
    java = Path(os.environ["JAVA_HOME"])
    java_system = {"linux": "linux", "darwin": "darwin", "win32": "win32"}[system]
    output = BUILD / "native" / target / JNI[system]
    compiler = os.environ.get("CC", "cc" if system != "win32" else "gcc")
    flags = ["-dynamiclib", "-mmacosx-version-min=15.0"] if system == "darwin" else ["-shared"]
    if system != "win32":
        flags += ["-fPIC"]
    if system == "linux":
        flags += ["-ldl"]
    env = dict(os.environ)
    if system == "darwin":
        env["MACOSX_DEPLOYMENT_TARGET"] = "15.0"
    run(compiler, "-Iinclude/WendyCore", "-Iinternal/bridge", f"-I{java / 'include'}", f"-I{java / 'include' / java_system}", "distributions/kotlin/native/bridge.c", *flags, "-o", output, env=env)
    stage_native(target, BUILD / "native" / target)


def wasm():
    check_version()
    stage_licenses()
    output = JS / "assets/wasm"
    output.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOOS": "js", "GOARCH": "wasm", "CGO_ENABLED": "0"}
    run("go", "build", "-trimpath", "-ldflags=" + go_ldflags(), "-o", output / "wendy_core.wasm", "./cmd/wendy-core-wasm", env=env)
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    copy(goroot / "lib/wasm/wasm_exec.js", output / "wasm_exec.mjs")
    copy(go_license(goroot), output / "LICENSE-Go.txt")
    files = {name: digest(output / name) for name in ("wendy_core.wasm", "wasm_exec.mjs", "LICENSE-Go.txt")}
    (output / "manifest.json").write_text(json.dumps({"version": version(), "identity": identity(), "target": "browser-wasm", "files": files}, indent=2) + "\n")


def inventory():
    target = host_target()
    directory = BUILD / "native" / target
    expected = (LIBRARIES[target.split("-")[0]], JNI[target.split("-")[0]], "wendy_core.node", "LICENSE-Go.txt", *SWIFT_FILES.get(target, ()))
    if target.startswith("darwin-"):
        for name in expected[:3]:
            commands = subprocess.check_output(["otool", "-l", str(directory / name)], text=True)
            minimum = re.search(r"minos (\d+)\.(\d+)", commands)
            if minimum is None or tuple(int(x) for x in minimum.groups()) > (15, 0):
                raise ValueError(f"{name} does not meet the declared macOS 15 minimum")
    files = {name: digest(directory / name) for name in expected}
    (directory / "manifest.json").write_text(json.dumps({"version": check_version(), "identity": identity(), "target": target, "files": files}, indent=2) + "\n")


def assemble(artifacts):
    check_version()
    # Validate everything before replacing the staged package resources.
    for target in TARGETS:
        directory = artifacts / ("native-" + target)
        manifest = json.loads((directory / "manifest.json").read_text())
        if manifest["target"] != target or manifest["version"] != version() or manifest.get("identity") != identity():
            raise ValueError(f"Wrong native artifact identity: {target}")
        expected = {LIBRARIES[target.split("-")[0]], JNI[target.split("-")[0]], "wendy_core.node", "LICENSE-Go.txt", *SWIFT_FILES.get(target, ())}
        if set(manifest["files"]) != expected:
            raise ValueError(f"Incomplete native artifact: {target}")
        for name, sha in manifest["files"].items():
            if digest(directory / name) != sha:
                raise ValueError(f"Native checksum mismatch: {target}/{name}")
    wasm_directory = artifacts / "wasm"
    wasm_manifest = json.loads((wasm_directory / "manifest.json").read_text())
    if wasm_manifest["version"] != version() or wasm_manifest["target"] != "browser-wasm" or wasm_manifest.get("identity") != identity():
        raise ValueError("Wrong WASM artifact identity")
    if set(wasm_manifest["files"]) != {"wendy_core.wasm", "wasm_exec.mjs", "LICENSE-Go.txt"}:
        raise ValueError("Incomplete WASM artifact")
    for name, sha in wasm_manifest["files"].items():
        if digest(wasm_directory / name) != sha:
            raise ValueError(f"WASM checksum mismatch: {name}")
    for destination in (JS / "assets", KOTLIN / "src/main/resources/native"):
        shutil.rmtree(destination, ignore_errors=True)
    for target in TARGETS:
        stage_native(target, artifacts / ("native-" + target))
    for name in (*wasm_manifest["files"], "manifest.json"):
        copy(wasm_directory / name, JS / "assets/wasm" / name)


def archive_version(archive):
    content = (version() + "\n").encode()
    entry = tarfile.TarInfo("VERSION")
    entry.size = len(content)
    archive.addfile(entry, io.BytesIO(content))


def pack_npm(destination):
    stage_licenses()
    destination.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as temp:
        directory = Path(temp)
        package = json.loads((JS / "package.json").read_text())
        for name in package["files"]:
            source = JS / name
            if source.is_dir():
                shutil.copytree(source, directory / name)
            else:
                copy(source, directory / name)
        package["version"] = version()
        (directory / "package.json").write_text(json.dumps(package, indent=2) + "\n")
        npm = "npm.cmd" if platform.system() == "Windows" else "npm"
        run(npm, "pack", "--ignore-scripts", "--pack-destination", destination, cwd=directory)
    return destination / f"wendylabs-core-{version()}.tgz"


def wheel():
    check_version()
    stage_licenses()
    with tempfile.TemporaryDirectory() as temp:
        directory = Path(temp) / "python"
        shutil.copytree(PYTHON, directory, ignore=shutil.ignore_patterns("build", "*.egg-info", "__pycache__"))
        metadata = directory / "pyproject.toml"
        metadata.write_text(metadata.read_text().replace(f'version = "{python_release_version(base_version())}"', f'version = "{identity()["pythonVersion"]}"'))
        environment = dict(os.environ)
        if platform.system() == "Darwin":
            environment["MACOSX_DEPLOYMENT_TARGET"] = "15.0"
        run(os.sys.executable, "-m", "build", "--wheel", "--no-isolation", "--outdir", BUILD / "wheels", directory, env=environment)


def swift_release(artifacts, release):
    """Name the Swift binaries as release assets; SwiftPM checksums are SHA-256."""
    copy(artifacts / "native-darwin-arm64/CWendyCore.xcframework.zip", release / f"CWendyCore-{version()}.xcframework.zip")
    with tempfile.TemporaryDirectory() as temp:
        bundle = Path(temp) / "CWendyCore.artifactbundle"
        libraries = {target: artifacts / ("native-" + target) / "libCWendyCore.a" for target in SWIFT_TRIPLES}
        artifact_bundle(libraries, bundle, artifacts / "native-linux-x64/LICENSE-Go.txt")
        zip_directory(bundle, release / f"CWendyCore-{version()}.artifactbundle.zip")


def pack(artifacts):
    check_version()
    stage_licenses()
    release = BUILD / "release"
    shutil.rmtree(release, ignore_errors=True)
    release.mkdir(parents=True)
    pack_npm(release)
    for target in TARGETS:
        system = target.split("-")[0]
        directory = artifacts / ("native-" + target)
        with tarfile.open(release / f"wendy-core-{version()}-{target}.tar.gz", "w:gz") as archive:
            archive.add(directory / LIBRARIES[system], arcname="lib/" + LIBRARIES[system])
            archive.add(directory / "LICENSE-Go.txt", arcname="LICENSE-Go.txt")
            archive.add(ROOT / "include", arcname="include")
            archive.add(ROOT / "proto", arcname="proto")
            archive_version(archive)
            for name in ("LICENSE", "NOTICE"):
                archive.add(ROOT / name, arcname=name)
    swift_release(artifacts, release)
    for target in TARGETS:
        wheels = list((artifacts / ("wheel-" + target)).glob("*.whl"))
        if len(wheels) != 1:
            raise ValueError(f"Every native target must contribute exactly one Python wheel: {target}")
        copy(wheels[0], release / wheels[0].name)
    if len(list(release.glob("*.whl"))) != len(TARGETS):
        raise ValueError("Python wheel filenames must identify distinct targets")
    (release / "SHA256SUMS").write_text("".join(f"{digest(path)}  {path.name}\n" for path in sorted(release.iterdir()) if path.is_file()))
    (release / "release.json").write_text(json.dumps({"version": version(), "identity": identity(), "targets": TARGETS, "javascript": ["node", "browser"], "goModule": "github.com/wendylabsinc/WendyOS/go/wendy-core/core"}, indent=2) + "\n")


def seal():
    release = BUILD / "release"
    documentation = release / "documentation"
    if documentation.exists():
        shutil.make_archive(str(release / "documentation"), "zip", documentation)
        shutil.rmtree(documentation)
    sums = release / "SHA256SUMS"
    sums.write_text("".join(f"{digest(path)}  {path.relative_to(release).as_posix()}\n" for path in sorted(release.rglob("*")) if path.is_file() and path != sums))


def test_packed_npm():
    npm = "npm.cmd" if platform.system() == "Windows" else "npm"
    package_directory = BUILD / "npm-smoke"
    package_directory.mkdir(parents=True, exist_ok=True)
    package = pack_npm(package_directory)
    with tempfile.TemporaryDirectory() as temp:
        path = Path(temp)
        (path / "package.json").write_text('{"type":"module","private":true}')
        run(npm, "install", "--ignore-scripts", "--no-audit", "--no-fund", package, cwd=path)
        conformance = (JS / "tests/conformance.mjs").as_uri()
        run("node", "--input-type=module", "-e", f'import * as core from "@wendylabs/core"; import {{assertCore}} from "{conformance}"; assertCore(core);', cwd=path)
        env = {**os.environ, "CORE_PACKAGE_ROOT": str(path / "node_modules/@wendylabs/core")}
        npx = "npx.cmd" if platform.system() == "Windows" else "npx"
        run(npx, "--no-install", "playwright", "test", cwd=JS, env=env)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["version", "native", "swift", "node", "jni", "wasm", "wheel", "inventory", "assemble", "pack", "seal", "test-npm"])
    parser.add_argument("--tag")
    parser.add_argument("--artifacts", type=Path, default=BUILD / "artifacts")
    arguments = parser.parse_args()
    if arguments.command == "version":
        print(check_version(arguments.tag))
    elif arguments.command in ("assemble", "pack"):
        globals()[arguments.command](arguments.artifacts)
    else:
        {"native": native, "swift": swift, "node": node_addon, "jni": jni, "wasm": wasm, "wheel": wheel, "inventory": inventory, "test-npm": test_packed_npm, "seal": seal}[arguments.command]()


if __name__ == "__main__":
    main()
