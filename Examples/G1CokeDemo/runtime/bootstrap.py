"""Prepare the Jetson runtime, then exec the selected fail-closed service.

Mirrors the qualified coke-segmentation bootstrap: Wendy's G1 GPU mount
exposes a zero-byte libnvdla_compiler.so, so the checksum-pinned JetPack
library baked into the image is unpacked into the writable container overlay
and its directory is prepended to LD_LIBRARY_PATH before exec. Without this
the CUDA driver shim loads and torch fails with error 801.

No runtime download is allowed, and this module sends no motor command.
"""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import subprocess
import sys

DLA_SHA256 = "928170c9e743257c063c235a610473cfec4be917a36f91a61a890ba2bfc47c94"


def ensure_dla_compiler(root: Path) -> Path:
    library = root / "usr/lib/aarch64-linux-gnu/nvidia/libnvdla_compiler.so"
    # Always extract from the verified package; an existing host file is untrusted.
    packaged = Path("/opt/vendor/nvidia-l4t-dla-compiler.deb")
    if not packaged.is_file():
        raise RuntimeError("pinned JetPack package missing from image")
    if hashlib.sha256(packaged.read_bytes()).hexdigest() != DLA_SHA256:
        raise RuntimeError("JetPack DLA compiler package checksum mismatch")
    root.mkdir(parents=True, exist_ok=True)
    subprocess.run(["dpkg-deb", "-x", str(packaged), str(root)], check=True)
    if not library.is_file() or library.stat().st_size <= 1_000_000:
        raise RuntimeError(f"DLA compiler extraction did not produce {library}")
    return library


def main() -> None:
    runtime_root = Path(
        os.environ.get("JETSON_RUNTIME_ROOT", "/opt/nvidia/jetpack-36.4.3")
    )
    ensure_dla_compiler(runtime_root)
    library_dir = str(runtime_root / "usr/lib/aarch64-linux-gnu/nvidia")
    environment = dict(os.environ)
    current = environment.get("LD_LIBRARY_PATH", "")
    environment["LD_LIBRARY_PATH"] = f"{library_dir}:{current}" if current else library_dir
    mode = environment.get("G1_RUNTIME_MODE", "shadow")
    module = {
        "shadow": "runtime.shadow_service",
        "benchmark": "runtime.policy_benchmark",
        "inference": "runtime.inference_service",
        "physical_probe": "physical_io.service",
        "physical_policy": "runtime.physical_policy_service",
    }.get(mode)
    if module is None:
        raise RuntimeError(f"unsupported G1_RUNTIME_MODE: {mode}")
    os.execvpe(
        sys.executable,
        [sys.executable, "-m", module],
        environment,
    )


if __name__ == "__main__":
    main()
