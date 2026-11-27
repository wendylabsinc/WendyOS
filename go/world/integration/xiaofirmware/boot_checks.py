"""Execute pinned firmware with and without a simulated camera peripheral.

This checks instruction execution and camera/DMA initialization. SensorLink
delivery and World publication require separate downstream capture evidence.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import time


def sha(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--emulator", type=Path, required=True)
    parser.add_argument("--prepared", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    emulator, prepared, out = (p.resolve() for p in (args.emulator, args.prepared, args.out))
    if out.is_relative_to(Path(__file__).resolve().parents[2]):
        parser.error("runtime artifacts must be outside the checkout")
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    inputs = json.loads((prepared / "inputs.json").read_text())
    for name, field in (("flash.bin", "flash_sha256"), ("firmware.elf", "elf_sha256")):
        if sha(prepared / name) != inputs[field]:
            raise ValueError("prepared firmware hash changed")
    stimuli = {}
    for item in inputs["stimuli"]:
        path = (prepared / item["path"]).resolve()
        if not path.is_relative_to(prepared) or sha(path) != item["sha256"]:
            raise ValueError("sensor stimulus changed")
        stimuli[item["sha256"]] = path
    cases = []
    common = [str(emulator), "--chip", "esp32s3", "--firmware", str(prepared / "flash.bin"),
              "--elf", str(prepared / "firmware.elf"), "--net", "user,restrict=yes",
              "--psram-size", "8M", "--timeout", "15s", "--log-color", "never"]
    for name, extra in (
        ("missing-sensor", []),
        ("ov3660", ["--i2c-dev", "1:0x3c,width=2,fill=0,reg=0x300a=0x36,reg=0x300b=0x60",
                    "--camera-frames", str(prepared / "stimuli"), "--camera-fps", "5",
                    "--camera-mjpeg", str(out / "dma.mjpeg")]),
    ):
        command = common + extra
        start = time.monotonic()
        with (out / (name + ".log")).open("wb") as log:
            result = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                                    timeout=25, env=dict(os.environ, ESP_EMU_DISABLE_AUTOUPDATES="1"))
        text = (out / (name + ".log")).read_text(errors="replace")
        checks = {"normal_boot": "Calling app_main()" in text, "management_started": "listening on port 5054" in text,
                  "exit_success": result.returncode == 0}
        if name == "missing-sensor":
            checks["camera_refused"] = "camera unavailable: ESP_ERR_NOT_SUPPORTED" in text and "camera ready:" not in text
        else:
            checks["camera_initialized"] = "camera ready: PID=0x3660 2048x1536 JPEG quality 12" in text
            checks["two_psram_buffers"] = text.count("Allocating 629145 Byte frame buffer in PSRAM") == 2
        cases.append({"case": name, "command": command, "wall_seconds": round(time.monotonic()-start, 3),
                      "checks": checks, "log_sha256": sha(out / (name + ".log"))})
    from PIL import Image
    data = (out / "dma.mjpeg").read_bytes()
    frames, position = [], 0
    while position < len(data):
        if data[position:position+2] != b"\xff\xd8":
            raise ValueError("non-JPEG bytes in DMA output")
        end = data.find(b"\xff\xd9", position + 2)
        if end < 0:
            raise ValueError("truncated DMA frame")
        frame = data[position:end+2]
        digest = hashlib.sha256(frame).hexdigest()
        if digest not in stimuli:
            raise ValueError("DMA output does not match a supplied sensor frame")
        with Image.open(io.BytesIO(frame)) as image:
            image.load()
            if image.size != (2048, 1536):
                raise ValueError("unexpected DMA dimensions")
        frames.append(digest)
        position = end + 2
    checks = {"all_distinct_sensor_inputs_reached_dma": len(set(frames)) == len(stimuli) and len(stimuli) >= 3,
              "multiple_frames": len(frames) >= len(stimuli), "flash_unchanged": sha(prepared / "flash.bin") == inputs["flash_sha256"]}
    passed = all(checks.values()) and all(all(c["checks"].values()) for c in cases)
    receipt = {"schema": "wendy.xiao-firmware-boot-checks.v1", "origin": "simulated", "passed": passed,
               "emulator_sha256": sha(emulator), "firmware": inputs, "cases": cases,
               "dma": {"frames": len(frames), "unique_frames": len(set(frames)), "sha256": sha(out / "dma.mjpeg")},
               "checks": checks, "sensorlink_delivery_verified": False, "physical_accuracy_verified": False}
    (out / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps({"passed": passed, "dma_frames": len(frames), "receipt": str(out / "receipt.json")}))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
