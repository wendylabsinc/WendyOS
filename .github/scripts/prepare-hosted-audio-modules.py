#!/usr/bin/env python3
"""Prepare real ALSA modules on the disposable media runner; never load them.

Ubuntu Azure 7.0 disables SOUND; AWS 7.0 keeps soundcore but disables ALSA:
https://packages.ubuntu.com/source/noble-updates/linux-azure-7.0
https://packages.ubuntu.com/source/noble-updates/linux-aws-7.0
Use its exact source + Ubuntu patches and installed ABI headers/Module.symvers.
The missing modules are built together with upstream kbuild, so modpost checks both
kernel imports and ALSA imports. No kernel/header config files are modified.
Actual loading and PCM verification belong to test-sensorlink-audio-loopback.sh.
"""
import gzip
import hashlib
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

# Source hashes and sizes are from each exact Ubuntu-signed .dsc.
# AWS retains the distribution's soundcore; Azure has no sound stack at all.
PINS = {
    "7.0.0-1017-azure": {
        "source": "linux-azure-7.0", "version": "7.0.0-1017.17~24.04.1",
        "patch_size": 4121122,
        "patch_sha256": "067230d08d88c8cb4218a9b154c78d060b61ec925961cc08db1e42af07a26635",
        "build_soundcore": True,
    },
    "7.0.0-1014-aws": {
        "source": "linux-aws-7.0", "version": "7.0.0-1014.14~24.04.1",
        "patch_size": 5011007,
        "patch_sha256": "9742a626b2eb81b9fd8ae9e083b4b4c4c96f161c021bd6f838273ea0a95ef581",
        "build_soundcore": False,
    },
}
ORIGINAL = (254937830, "c6343795c8c7e4feff55f5b309365c8e0eaf49cd6abcb412e32315313f149536")


def archives(pin):
    return {
        pin["source"] + "_7.0.0.orig.tar.gz": ORIGINAL,
        pin["source"] + "_" + pin["version"] + ".diff.gz": (pin["patch_size"], pin["patch_sha256"]),
    }


MODULES = ("soundcore", "snd", "snd-timer", "snd-pcm", "snd-aloop")
CONFIG = {"SOUND": "m", "SND": "m", "SND_TIMER": "m", "SND_PCM": "m",
          "SND_ALOOP": "m", "SND_PCM_TIMER": "y", "SND_PROC_FS": "y",
          "SND_VERBOSE_PROCFS": "y"}


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, timeout=300, **kwargs)


def host_guard():
    if (os.environ.get("GITHUB_ACTIONS") != "true"
            or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
            or platform.system() != "Linux"):
        raise ValueError("Audio module preparation requires the disposable GitHub-hosted Linux runner")


def validate_headers(headers, release, machine, version):
    pin = PINS.get(release)
    if pin is None or machine != "x86_64" or version != pin["version"]:
        raise ValueError(f"No audited ALSA module source for {release}/{machine}/{version}; update the exact-source pin")
    if (headers / "include/config/kernel.release").read_text().strip() != release:
        raise ValueError("Installed headers do not match the running kernel")
    config = (headers / ".config").read_text().splitlines()
    expected_sound = "# CONFIG_SOUND is not set" if pin["build_soundcore"] else "CONFIG_SOUND=m"
    if expected_sound not in config or (not pin["build_soundcore"] and "# CONFIG_SND is not set" not in config):
        raise ValueError("Kernel sound configuration differs from the audited source")
    if "CONFIG_MODULES=y" not in config or "CONFIG_MODVERSIONS=y" not in config:
        raise ValueError("Kernel module loading and symbol version checks must be enabled")
    if "CONFIG_MODULE_SIG_FORCE=y" in config:
        raise ValueError("Kernel requires signed modules; unsigned fixture modules cannot be used")
    if not (headers / "Module.symvers").stat().st_size:
        raise ValueError("Matching kernel symbol versions are missing")
    return pin


def verify_archive(path, expected):
    size, digest = expected
    with path.open("rb") as stream:
        checksum = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            checksum.update(chunk)
        actual = checksum.hexdigest()
    if path.stat().st_size != size or actual != digest:
        raise ValueError(f"Ubuntu source checksum/size mismatch: {path.name}")


def extract_sound(archive, destination):
    with tarfile.open(archive) as source:
        members = []
        for member in source.getmembers():
            parts = PurePosixPath(member.name).parts
            if len(parts) < 2 or parts[:2] != ("linux-7.0", "sound"):
                continue
            if ".." in parts or not (member.isfile() or member.isdir()):
                raise ValueError("Unexpected path or special file in Ubuntu sound source")
            members.append(member)
        if not members:
            raise ValueError("Ubuntu archive contains no sound source")
        for member in members:
            target = destination / member.name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with source.extractfile(member) as src, target.open("xb") as out:
                    shutil.copyfileobj(src, out)
    return destination / "linux-7.0" / "sound"


def sound_patch(diff, pin):
    prefix = pin["source"] + "-7.0.0"
    chunks = re.split(r"(?m)(?=^--- " + re.escape(prefix) + r"(?:\.orig)?/)", diff)
    selected = [chunk for chunk in chunks
                if chunk.startswith("--- " + prefix + ".orig/sound/")]
    if not selected:
        raise ValueError("Ubuntu patchset contains no sound patches")
    return "".join(selected)


def build_command(headers, sound, pin):
    config = dict(CONFIG)
    if not pin["build_soundcore"]:
        del config["SOUND"]
    definitions = [f"-DCONFIG_{key}{'_MODULE' if value == 'm' else ''}=1"
                   for key, value in config.items()]
    return ["make", "-j2", "-C", str(headers), f"M={sound}",
            "CC=x86_64-linux-gnu-gcc-13",
            *[f"CONFIG_{key}={value}" for key, value in config.items()],
            *(["CONFIG_SOUND="] if not pin["build_soundcore"] else []),
            "KCFLAGS=" + " ".join(definitions), "modules"]


def required_modules(pin):
    return MODULES if pin["build_soundcore"] else MODULES[1:]


def require_empty_stack(pin):
    for name in MODULES:
        if (Path("/sys/module") / name.replace("-", "_")).exists():
            raise ValueError(f"Refusing to replace loaded module {name}")
        if name in required_modules(pin) and subprocess.run(["modinfo", name], capture_output=True, timeout=10).returncode == 0:
            raise ValueError(f"Refusing to shadow existing module {name}")


def validate_packaged_soundcore(release, version):
    expected = Path("/lib/modules") / release / "kernel/sound/soundcore.ko.zst"
    resolved = run(["modinfo", "-k", release, "-n", "soundcore"], capture_output=True).stdout.strip()
    if Path(resolved).resolve() != expected.resolve() or not expected.is_file():
        raise ValueError("AWS soundcore must resolve to its distribution module, not an override")
    package = "linux-modules-" + release
    owner = run(["dpkg-query", "-S", str(expected)], capture_output=True).stdout.strip()
    installed_version = run(["dpkg-query", "-W", "-f=${Version}", package], capture_output=True).stdout
    magic = run(["modinfo", "-F", "vermagic", str(expected)], capture_output=True).stdout
    if (owner != package + ": " + str(expected) or installed_version != version
            or not magic.split() or magic.split()[0] != release or "modversions" not in magic.split()):
        raise ValueError("AWS soundcore is not owned by the exact matching Ubuntu module package")


def main():
    host_guard()  # Before subprocesses, downloads, filesystem setup or sudo.
    if subprocess.run(["modinfo", "snd-aloop"], capture_output=True, timeout=10).returncode == 0:
        print("Using the runner's packaged snd-aloop module")
        return
    release = platform.release()
    headers = Path("/lib/modules") / release / "build"
    version = run(["dpkg-query", "-W", "-f=${Version}", f"linux-headers-{release}"],
                  capture_output=True).stdout
    pin = validate_headers(headers, release, platform.machine(), version)
    require_empty_stack(pin)
    if not pin["build_soundcore"]:
        validate_packaged_soundcore(release, version)
    installed = Path("/lib/modules") / release / "updates/wendy-audio-regression"
    if installed.exists() or installed.is_symlink():
        raise ValueError("Refusing to reuse an existing audio fixture installation")
    output = Path(tempfile.mkdtemp(prefix="wendy-audio-modules-", dir=os.environ["RUNNER_TEMP"]))
    print(f"Exact-source ALSA build evidence: {output}", flush=True)
    source_archives = archives(pin)
    base = "https://archive.ubuntu.com/ubuntu/pool/main/l/" + pin["source"] + "/"
    for name, expected in source_archives.items():
        run(["curl", "--fail", "--location", "--proto", "=https", "--proto-redir", "=https",
             "--connect-timeout", "15", "--max-time", "180", "--output", str(output / name), base + name])
        verify_archive(output / name, expected)
    sound = extract_sound(output / next(iter(source_archives)), output)
    diff = gzip.decompress((output / (pin["source"] + "_" + pin["version"] + ".diff.gz")).read_bytes()).decode()
    run(["patch", "--batch", "--forward", "-p1", "-d", str(sound.parent)], input=sound_patch(diff, pin))
    with (output / "build.log").open("w") as log:
        run(build_command(headers, sound, pin), stdout=log, stderr=subprocess.STDOUT)
    modules = sorted(sound.rglob("*.ko"))
    if {module.stem for module in modules} != set(required_modules(pin)) or len(modules) != len(required_modules(pin)):
        raise ValueError("Build did not produce exactly the audited ALSA fixture modules")
    for module in modules:
        magic = run(["modinfo", "-F", "vermagic", str(module)], capture_output=True).stdout
        if magic.split()[0] != release or "modversions" not in magic.split():
            raise ValueError(f"Incorrect kernel ABI for {module.name}: {magic}")
    # All source/config/symbol validation is complete before the first mutation
    # outside RUNNER_TEMP. This directory is unique to the disposable CI fixture.
    require_empty_stack(pin)  # Recheck after the bounded download/build interval.
    if not pin["build_soundcore"]:
        validate_packaged_soundcore(release, version)
    run(["sudo", "mkdir", "-p", str(installed.parent)])
    run(["sudo", "mkdir", str(installed)])
    for module in modules:
        run(["sudo", "install", "-m", "644", str(module), str(installed / module.name)])
    run(["sudo", "depmod", "-a", release])
    for name in required_modules(pin):
        resolved = run(["modinfo", "-n", name], capture_output=True).stdout.strip()
        if Path(resolved).resolve() != (installed / f"{name}.ko").resolve():
            raise ValueError(f"Module lookup does not select this fixture: {name}: {resolved}")
    print("Exact-source ALSA modules prepared; actual loading and PCM checks are still required")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(error, file=sys.stderr)
        sys.exit(2)
