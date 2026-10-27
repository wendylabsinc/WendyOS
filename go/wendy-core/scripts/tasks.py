"""Implementation of Make tasks; Make owns only task dependencies."""
import argparse
import os
import platform
import shutil

import build
from build import BUILD, ROOT, run
from versioning import base_version


def descriptor():
    directory = BUILD / "sdk"
    directory.mkdir(parents=True, exist_ok=True)
    run("protoc", "-I", "proto", "--include_imports", "--include_source_info",
        f"--descriptor_set_out={directory / 'wendy-core.pb'}", "proto/wendy/core/v1/core.proto")


def manifest():
    run("go", "run", "./cmd/wendy-core-docs", "snapshot", "--headers", "include/WendyCore",
        "--descriptor", BUILD / "sdk/wendy-core.pb", "--version", base_version(),
        "--output", BUILD / "sdk/api.json", "--markdown", BUILD / "sdk/protobuf-reference.md")


def generate():
    run(os.sys.executable, ROOT / "scripts/generate.py")


def check():
    run("go", "test", "./...")
    run("go", "vet", "./...")
    run(os.sys.executable, "-m", "unittest", "discover", "-s", "scripts", "-p", "test_*.py", "-v")
    run("clang++", "-x", "c++", "-std=c++17", "-fsyntax-only", "-Iinclude", "examples/c/hello.c")
    for compiler, language, standard in (("clang", "c", "c11"), ("clang++", "c++", "c++17")):
        run(compiler, "-x", language, f"-std={standard}", "-fsyntax-only", "-Iinclude", "-",
            input=b"#include <WendyCore/WendyPlatform.h>\n")


def javascript():
    npm = "npm.cmd" if platform.system() == "Windows" else "npm"
    npx = "npx.cmd" if platform.system() == "Windows" else "npx"
    run(npm, "ci", cwd=build.JS)
    build.node_addon()
    run(npx, "playwright", "install", "chromium", cwd=build.JS)
    run(npm, "test", cwd=build.JS)
    run(npm, "run", "test:types", cwd=build.JS)
    build.test_packed_npm()


def python():
    run(os.sys.executable, "-m", "unittest", "discover", "-s", "distributions/python/tests", "-v",
        env={**os.environ, "PYTHONPATH": "distributions/python"})


def kotlin():
    gradle = shutil.which("gradle")
    if gradle is None:
        raise SystemExit("Gradle 9.8.1 is required for Kotlin builds; install it and add gradle to PATH.")
    build.jni()
    run(gradle, "-p", build.KOTLIN, "test", "--no-daemon")


def swift():
    build.swift()
    if platform.system() == "Darwin":
        run("xcodebuildmcp", "swift-package", "test", "--package-path", ROOT)
        # Compile and link (without running) the tests against the iOS slices.
        for destination in ("generic/platform=iOS", "generic/platform=iOS Simulator"):
            run("xcodebuild", "build-for-testing", "-quiet", "-scheme", "wendy-core", "-destination", destination,
                "-derivedDataPath", BUILD / "swift/ios", env=build.apple_environment())
    else:
        run("swift", "test", cwd=ROOT)


def compare():
    if (ROOT / f"documentation/versions/{base_version()}.json").read_bytes() != (BUILD / "sdk/api.json").read_bytes():
        raise ValueError("Generated contract differs from its committed base-version snapshot")


def documentation():
    run("doxygen", "Doxyfile")


def main():
    tasks = {"descriptor": descriptor, "manifest": manifest, "generate": generate,
             "check": check, "javascript": javascript, "python": python, "kotlin": kotlin,
             "swift": swift, "compare": compare, "documentation": documentation}
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("task", choices=tasks)
    tasks[parser.parse_args().task]()


if __name__ == "__main__":
    main()
