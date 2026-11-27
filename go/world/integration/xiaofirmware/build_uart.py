#!/usr/bin/env python3
"""Build a private UART transport variant without editing the retained source.

Requires the already installed ESP-IDF 6.0.3 toolchain and existing managed
components. This changes management transport only. It never flashes hardware.
The original USB image remains a separate firmware-execution test target.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess


EXCLUDED = {"build", ".git", "__pycache__", "sdkconfig", "sdkconfig.old"}


def sha256(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def manifest(root):
    result = {}
    for directory, dirs, files in os.walk(root):
        dirs[:] = sorted(d for d in dirs if d not in EXCLUDED)
        for name in sorted(files):
            if name not in EXCLUDED:
                p = Path(directory) / name
                result[str(p.relative_to(root))] = sha256(p)
    return result


def config(path):
    values = {}
    for line in path.read_text().splitlines():
        if line.startswith("CONFIG_") and "=" in line:
            key, value = line.split("=", 1)
            values[key] = value
        elif line.startswith("# CONFIG_") and line.endswith(" is not set"):
            values[line[2:-11]] = "n"
    return values


def dependency_constraints(python, lock):
    script = ("import json,sys,yaml; d=yaml.safe_load(open(sys.argv[1])); "
              "print(';'.join(k+'=='+v['version'] for k,v in d['dependencies'].items() "
              "if v['source']['type']=='service'))")
    return subprocess.check_output([str(python), "-c", script, str(lock)], text=True).strip()


def make_receipt(original, copied, build, flash, commands, environment):
    original_map = manifest(original)
    copied_map = manifest(copied)
    changed = [p for p in sorted(original_map.keys() | copied_map.keys())
               if original_map.get(p) != copied_map.get(p)]
    if changed:
        raise RuntimeError(f"source files changed beyond sdkconfig: {changed}")
    before, after = config(original / "sdkconfig"), config(copied / "sdkconfig")
    differences = {k: {"before": before.get(k), "after": after.get(k)}
                   for k in sorted(before.keys() | after.keys()) if before.get(k) != after.get(k)}
    allowed = {"CONFIG_WENDY_USJ_USE_UART", "CONFIG_WENDY_USJ_UART_TX_GPIO", "CONFIG_WENDY_USJ_UART_RX_GPIO"}
    if set(differences) - allowed or after.get("CONFIG_WENDY_USJ_USE_UART") != "y":
        raise RuntimeError(f"unexpected firmware configuration changes: {differences}")
    artifacts = {"flash": flash, "app": build / "wendy_mcu.bin", "elf": build / "wendy_mcu.elf",
                 "bootloader": build / "bootloader/bootloader.bin",
                 "partition_table": build / "partition_table/partition-table.bin",
                 "otadata": build / "ota_data_initial.bin"}
    return {"schema": "wendy.xiao-uart-build.v1", "hardware_flashed": False,
            "firmware_variant": "production UART0 management transport; camera code unchanged",
            "original_source": str(original), "copied_source": str(copied),
            "source_files_verified": len(original_map), "source_differences": changed,
            "source_manifest_sha256": hashlib.sha256(json.dumps(original_map, sort_keys=True).encode()).hexdigest(),
            "config_changes": differences, "sdkconfig_sha256": sha256(copied / "sdkconfig"),
            "artifacts": {key: {"path": str(p), "sha256": sha256(p), "bytes": p.stat().st_size}
                          for key, p in artifacts.items()},
            "commands": commands, "build_environment": environment,
            "transport_limitation": "Original USB Serial/JTAG binary boot tested separately; USB host handshake unverified"}


def run(command, cwd, env, logfile, seconds):
    with logfile.open("ab") as log:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        try:
            result = process.wait(timeout=seconds)
        except BaseException:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)
            raise
        if result:
            raise RuntimeError(f"build command failed with exit {result}; see {logfile}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source", "out", "idf", "python", "toolchain-bin", "cmake-bin", "ninja-bin"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--seconds", type=int, default=600)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 1800:
        parser.error("build timeout must be between 1 and 1800 seconds")
    for name in ("source", "out", "idf", "toolchain_bin", "cmake_bin", "ninja_bin"):
        setattr(args, name, getattr(args, name).expanduser().resolve())
    # A venv's Python is a symlink; resolving it loses the venv site-packages.
    args.python = args.python.expanduser().absolute()
    checkout = Path(__file__).resolve().parents[2]
    if args.out.is_relative_to(checkout) or args.out.is_relative_to(args.source):
        parser.error("--out must be outside both the checkout and original firmware source")
    if args.out.exists() and any(args.out.iterdir()):
        parser.error("--out must be empty")
    values = config(args.source / "sdkconfig")
    for key in ("CONFIG_WENDY_WIFI_SSID", "CONFIG_WENDY_WIFI_PASSWORD"):
        if values.get(key, '""') != '""':
            parser.error("firmware source must have blank WiFi credentials")
    if values.get("CONFIG_IDF_TARGET") != '"esp32s3"':
        parser.error("firmware must target ESP32-S3")
    args.out.mkdir(parents=True, exist_ok=True, mode=0o700)
    copied, build, flash = args.out / "source", args.out / "build", args.out / "blank-flash.bin"
    shutil.copytree(args.source, copied,
                    ignore=shutil.ignore_patterns(".git", "build", "__pycache__", "sdkconfig.old"))
    sdkconfig = copied / "sdkconfig"
    text = sdkconfig.read_text()
    marker = "# CONFIG_WENDY_USJ_USE_UART is not set"
    if marker not in text:
        raise RuntimeError("expected original USB Serial/JTAG firmware configuration")
    sdkconfig.write_text(text.replace(marker, "CONFIG_WENDY_USJ_USE_UART=y"))
    # Existing IDF minimal-build dependency propagation omits UART HAL's public
    # include directory for wendy_usj. Add SDK headers without editing C source.
    overrides = {"IDF_PATH": str(args.idf), "ESP_IDF_VERSION": "6.0.3",
                 "IDF_PYTHON_CHECK_CONSTRAINTS": "no",
                 "IDF_COMPONENT_CHECK_NEW_VERSION": "false",
                 "IDF_COMPONENT_CONSTRAINTS": dependency_constraints(args.python, copied / "dependencies.lock"),
                 "IDF_PYTHON_ENV_PATH": str(args.python.parent.parent),
                 "C_INCLUDE_PATH": ":".join(str(args.idf / p) for p in
                      ("components/esp_hal_uart/include", "components/esp_hal_uart/esp32s3/include"))}
    env = dict(os.environ, **overrides)
    env["PATH"] = os.pathsep.join(map(str, (args.python.parent, args.toolchain_bin,
                                           args.cmake_bin, args.ninja_bin))) + os.pathsep + env.get("PATH", "")
    build_command = [str(args.python), str(args.idf / "tools/idf.py"), "-B", str(build),
                     "-D", "SDKCONFIG_DEFAULTS=sdkconfig.defaults;boards/seeed_xiao_esp32s3_native.defaults;version.defaults", "build"]
    run(build_command, copied, env, args.out / "build.log", args.seconds)
    flash_map = json.loads((build / "flasher_args.json").read_text())["flash_files"]
    merge = [str(args.python), "-m", "esptool", "--chip", "esp32s3", "merge-bin",
             "--format", "raw", "--pad-to-size", "8MB", "--output", str(flash)]
    for offset, relative in sorted(flash_map.items(), key=lambda item: int(item[0], 0)):
        merge += [offset, str(build / relative)]
    run(merge, copied, env, args.out / "build.log", 60)
    receipt = make_receipt(args.source, copied, build, flash, [build_command, merge], overrides)
    (args.out / "build-receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps({"receipt": str(args.out / "build-receipt.json"), "flash": str(flash)}))


if __name__ == "__main__":
    def interrupted(_signal, _frame):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    main()
