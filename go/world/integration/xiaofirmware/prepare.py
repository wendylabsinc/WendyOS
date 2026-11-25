"""Prepare blank XIAO flash and deterministic JPEG inputs outside Git.

No board flash dump, NVS data, enrollment identity or camera recording is read.
The firmware build is the retained, previously deployed full-resolution build.
"""
import argparse
import hashlib
import json
from pathlib import Path


APP_SHA256 = "23df961cddfbccc472bd0eb4dc3efe5c81c20e9c7221e3f4c4e551760bf1c19c"
INPUTS = {
    "bootloader/bootloader.bin": (0, "e40a5ad643db313a080f05183cd259b25220a6c6dd7c3840b30859d7ea5096fa"),
    "partition_table/partition-table.bin": (0x8000, "26b5f59dfc2e7873a57a143604c2660380388741a070e7c1fbe743012afc3520"),
    "wendy_mcu.bin": (0x10000, APP_SHA256),
    "ota_data_initial.bin": (0x7C0000, "7d2c7ac4888bfd75cd5f56e8d61f69595121183afc81556c876732fd3782c62f"),
}


def prepare(build, out):
    if out.resolve().is_relative_to(Path(__file__).resolve().parents[2]):
        raise ValueError("runtime artifacts must be outside the checkout")
    # Validate every input before creating output. Never merge arbitrary offsets
    # from a device dump or include persisted application/configuration data.
    data = {}
    for name, (offset, digest) in INPUTS.items():
        blob = (build / name).read_bytes()
        if hashlib.sha256(blob).hexdigest() != digest:
            raise ValueError(f"Pinned firmware input mismatch: {name}")
        data[name] = blob
    if set(data["ota_data_initial.bin"]) != {255}:
        raise ValueError("OTA initialization must be erased")
    elf = (build / "wendy_mcu.elf").read_bytes()
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    flash = bytearray(b"\xff" * (8 * 1024 * 1024))
    for name, (offset, _) in INPUTS.items():
        flash[offset:offset + len(data[name])] = data[name]
    (out / "flash.bin").write_bytes(flash)
    # The actual ELF is required for correct simulator symbol matching. It is
    # build output, not an extracted board identity or provisioning image.
    (out / "firmware.elf").write_bytes(elf)
    from PIL import Image, ImageDraw
    frames = out / "stimuli"
    frames.mkdir()
    stimulus = []
    # A longer prime-length sequence avoids a short repeating image cycle
    # aliasing with World's independent receipt sampling interval.
    for index in range(11):
        image = Image.new("RGB", (2048, 1536), (20, 30, 50))
        draw = ImageDraw.Draw(image)
        for y in range(0, 1536, 128):
            for x in range(0, 2048, 128):
                if (x // 128 + y // 128) % 2:
                    draw.rectangle((x, y, x + 127, y + 127), fill=(200, 200, 200))
        draw.rectangle((100 + index * 150, 300, 300 + index * 150, 600), fill=(220, 40, 50))
        draw.text((40, 50), f"SIMULATED SENSOR INPUT {index}", fill="white")
        path = frames / f"frame{index:03}.jpg"
        image.save(path, quality=85)
        stimulus.append({"path": str(path.relative_to(out)), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()})
    receipt = {
        "schema": "wendy.xiao-firmware-inputs.v1", "origin": "simulated",
        "firmware_source_commit": "6b8bc11b0ee5c48c7777136de2ccc684dbec4325",
        "firmware_overlay": "adapters/wendy-lite-camera/full-resolution.patch",
        "app_sha256": APP_SHA256,
        "flash_sha256": hashlib.sha256(flash).hexdigest(),
        "elf_sha256": hashlib.sha256(elf).hexdigest(),
        "storage": "8 MiB initially erased; only pinned build images merged; no device state",
        "inputs": [{"path": n, "offset": o, "sha256": h, "bytes": len(data[n])} for n, (o, h) in INPUTS.items()],
        "stimuli": stimulus,
        "sensor_model": "OV3660 identity and writable SCCB register file; supplied JPEG at DVP receiver",
        "not_modelled": ["optics", "exposure", "sensor JPEG encoder", "RF channel", "physical performance"],
    }
    (out / "inputs.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--firmware-build", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    prepare(args.firmware_build.resolve(), args.out.resolve())
    print(args.out / "inputs.json")


if __name__ == "__main__":
    main()
