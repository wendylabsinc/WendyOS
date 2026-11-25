"""Verify captured emulator output, without manufacturing firmware-execution proof.

Requires Pillow. Paths in capture.json and its frame rows are relative to the
capture directory. No images or firmware binaries belong in the source tree.
This reader never connects to a device or generates a camera stream.
"""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import sys
import warnings


SCHEMA = "wendy.xiao-firmware-capture.v1"
MAX_JPEG_BYTES = 8 * 1024 * 1024
MAX_PIXELS = 16 * 1024 * 1024
MAX_FRAMES = 100_000


class InvalidCapture(ValueError):
    pass


def integer(value, name, maximum):
    if type(value) is not int or not 0 <= value <= maximum:
        raise InvalidCapture(f"{name} is outside its integer bounds")
    return value


def label(value, name):
    if not isinstance(value, str) or not value or len(value) > 256:
        raise InvalidCapture(f"{name} must be a nonempty bounded string")
    return value


def local_file(root, value):
    if not isinstance(value, str) or not value or Path(value).is_absolute():
        raise InvalidCapture("artifact path must be relative")
    path = (root / value).resolve()
    if not path.is_relative_to(root) or not path.is_file():
        raise InvalidCapture("artifact path escapes the capture or is not a file")
    return path


def digest_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def checked_digest(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value):
        raise InvalidCapture("sha256 must be a lowercase SHA-256 digest")
    return value


def artifact(root, reference):
    if not isinstance(reference, dict):
        raise InvalidCapture("artifact reference must be an object")
    path = local_file(root, reference.get("path"))
    expected = checked_digest(reference.get("sha256"))
    if digest_file(path) != expected:
        raise InvalidCapture(f"artifact digest mismatch: {reference['path']}")
    return {"path": reference["path"], "sha256": expected, "bytes": path.stat().st_size}


def dimensions(value):
    width = integer(value.get("width"), "width", 8192)
    height = integer(value.get("height"), "height", 8192)
    if not width or not height or width * height > MAX_PIXELS:
        raise InvalidCapture("image dimensions exceed bounds")
    return width, height


def jpeg(root, row):
    try:
        from PIL import Image, ImageFile
    except ImportError as error:
        raise InvalidCapture("Pillow is required for full JPEG decoding") from error
    path = local_file(root, row.get("jpeg_path"))
    if not 4 <= path.stat().st_size <= MAX_JPEG_BYTES:
        raise InvalidCapture("JPEG byte length exceeds bounds")
    data = path.read_bytes()
    digest = checked_digest(row.get("sha256"))
    if hashlib.sha256(data).hexdigest() != digest:
        raise InvalidCapture("JPEG digest mismatch")
    if not data.startswith(b"\xff\xd8") or not data.endswith(b"\xff\xd9"):
        raise InvalidCapture("JPEG is missing its complete start/end markers")
    expected = dimensions(row)
    # Pillow's permissive truncated-image setting must not weaken this check.
    previous = ImageFile.LOAD_TRUNCATED_IMAGES
    ImageFile.LOAD_TRUNCATED_IMAGES = False
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("error", Image.DecompressionBombWarning)
            with Image.open(io.BytesIO(data)) as image:
                if image.format != "JPEG" or image.size != expected:
                    raise InvalidCapture("decoded JPEG dimensions or format differ")
                image.load()
    except InvalidCapture:
        raise
    except (OSError, ValueError, Image.DecompressionBombWarning,
            Image.DecompressionBombError) as error:
        raise InvalidCapture(f"JPEG decode failed: {error}") from error
    finally:
        ImageFile.LOAD_TRUNCATED_IMAGES = previous
    return digest, len(data)


def load_json(path, limit):
    if path.stat().st_size > limit:
        raise InvalidCapture("JSON document exceeds bounds")
    with path.open() as stream:
        value = json.load(stream)
    if not isinstance(value, dict):
        raise InvalidCapture("JSON document must be an object")
    return value


def verify(capture_path):
    capture_path = Path(capture_path).resolve()
    root = capture_path.parent
    capture = load_json(capture_path, 1024 * 1024)
    if capture.get("schema") != SCHEMA or capture.get("origin") != "simulated":
        raise InvalidCapture("capture must use the explicit simulated firmware schema")
    run_id = label(capture.get("run_id"), "run_id")
    if type(capture.get("capture_complete", False)) is not bool:
        raise InvalidCapture("capture_complete must be a boolean")
    firmware = artifact(root, capture.get("firmware"))
    emulator = artifact(root, capture.get("emulator"))
    emulator["declared_version"] = label(capture["emulator"].get("version"), "emulator version")
    stimuli = capture.get("stimuli")
    if not isinstance(stimuli, list) or not 1 <= len(stimuli) <= 10_000:
        raise InvalidCapture("capture requires a bounded list of sensor stimuli")
    stimuli = [artifact(root, item) for item in stimuli]
    channels = capture.get("channels")
    if not isinstance(channels, list) or not 1 <= len(channels) <= 8:
        raise InvalidCapture("capture requires one through eight camera channels")
    expected = {}
    for channel in channels:
        if not isinstance(channel, dict):
            raise InvalidCapture("channel must be an object")
        number = integer(channel.get("channel_id"), "channel_id", 2**32 - 1)
        if number in expected:
            raise InvalidCapture("duplicate channel")
        expected[number] = dimensions(channel)
    frames_path = local_file(root, capture.get("frames_path"))
    report = {
        "schema": "wendy.xiao-firmware-stream-report.v1",
        "run_id": run_id, "origin": "simulated",
        "capture_complete": capture.get("capture_complete", False),
        "firmware": firmware, "emulator": emulator, "stimuli": stimuli,
        "frames_jsonl_sha256": digest_file(frames_path),
        "artifact_integrity_verified": True,
        "firmware_execution_verified": False,
        "stream_integrity_verified": False,
        "rows": 0, "decoded_frames": 0, "decoded_bytes": 0,
        "missing_sequence_frames": 0, "duplicate_sequence_frames": 0,
        "out_of_order_frames": 0, "nonadvancing_timestamps": 0,
        "rejected_frames": [], "sessions": [], "distinct_jpegs": 0,
        "limits": [
            "Artifact hashes and declared metadata do not prove emulator execution; inspect the independent runtime receipt.",
            "Decoded frames do not establish that the sensor driver, DMA or Wi-Fi radio were emulated.",
            "Sequence gaps count missing frame numbers, not their cause. Chunk loss before this capture is not observable.",
            "No physical camera, optical calibration, RF behavior or hardware throughput is verified.",
        ],
    }
    states, hashes, sessions = {}, set(), set()
    with frames_path.open("rb") as stream:
        while line := stream.readline(64 * 1024 + 1):
            if len(line) > 64 * 1024 or report["rows"] >= MAX_FRAMES:
                raise InvalidCapture("frame record count or line length exceeds bounds")
            report["rows"] += 1
            try:
                row = json.loads(line)
                if not isinstance(row, dict):
                    raise InvalidCapture("frame must be an object")
                session = label(row.get("session_id"), "session_id")
                channel = integer(row.get("channel_id"), "channel_id", 2**32 - 1)
                if channel not in expected or dimensions(row) != expected[channel]:
                    raise InvalidCapture("frame differs from declared camera channel")
                sequence = integer(row.get("frame_seq"), "frame_seq", 2**32 - 1)
                timestamp = integer(row.get("timestamp_us"), "timestamp_us", 2**64 - 1)
                key = session, channel
                prior = states.get(key)
                if prior:
                    delta = (sequence - prior[0]) % 2**32
                    if delta == 0:
                        report["duplicate_sequence_frames"] += 1
                        raise InvalidCapture("duplicate frame sequence")
                    if delta >= 2**31:
                        report["out_of_order_frames"] += 1
                        raise InvalidCapture("out-of-order frame sequence")
                    if timestamp <= prior[1]:
                        report["nonadvancing_timestamps"] += 1
                        raise InvalidCapture("timestamp did not advance")
                    report["missing_sequence_frames"] += delta - 1
                # Fence observed source versions even when JPEG validation fails.
                states[key] = sequence, timestamp
                digest, size = jpeg(root, row)
                hashes.add(digest)
                sessions.add(session)
                report["decoded_frames"] += 1
                report["decoded_bytes"] += size
            except (InvalidCapture, ValueError, UnicodeError, OSError) as error:
                report["rejected_frames"].append({"row": report["rows"], "reason": str(error)})
    report["sessions"] = sorted(sessions)
    report["distinct_jpegs"] = len(hashes)
    report["stream_integrity_verified"] = report["decoded_frames"] > 0 and not report["rejected_frames"]
    if report["rows"] == 0:
        report["rejected_frames"].append({"row": 0, "reason": "no frame records"})
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capture", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    try:
        report = verify(args.capture)
    except (InvalidCapture, OSError, ValueError) as error:
        report = {"stream_integrity_verified": False, "firmware_execution_verified": False,
                  "error": str(error)}
    with args.out.open("x") as stream:
        json.dump(report, stream, indent=2)
        stream.write("\n")
    return 0 if report.get("stream_integrity_verified") else 1


if __name__ == "__main__":
    sys.exit(main())
