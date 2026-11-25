"""Actual RGB pose inference. Input is image bytes only, with no scene labels."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import sys

import numpy as np
from PIL import Image

try:
    from .fetch_model import METADATA, verify
except ImportError:
    from fetch_model import METADATA, verify

MAX_IMAGE_BYTES = 16 * 1024 * 1024
MAX_PIXELS = 4096 * 4096
MAX_REQUEST_BYTES = 16 * 1024
DEFAULT_SIZE = 384
SCORE_THRESHOLD = 0.2
ADAPTER_VERSION = "1"


def prepare_image(data, size=DEFAULT_SIZE):
    if not data or len(data) > MAX_IMAGE_BYTES:
        raise ValueError("image byte bound")
    if size not in (256, 384, 512, 640):
        raise ValueError("unsupported input size")
    with Image.open(io.BytesIO(data)) as source:
        width, height = source.size
        if not 1 <= width <= 4096 or not 1 <= height <= 4096 or width * height > MAX_PIXELS:
            raise ValueError("image dimension bound")
        # Preserve encoded pixel orientation. Calibration describes those pixels;
        # applying EXIF rotation here would silently change its coordinate frame.
        rgb = source.convert("RGB")
    scale = min(size / width, size / height)
    resized_width = max(1, round(width * scale))
    resized_height = max(1, round(height * scale))
    left = (size - resized_width) // 2
    top = (size - resized_height) // 2
    canvas = Image.new("RGB", (size, size))
    canvas.paste(rgb.resize((resized_width, resized_height), Image.Resampling.BILINEAR), (left, top))
    transform = {"width": width, "height": height, "size": size, "left": left, "top": top,
                 "scale_x": resized_width / width, "scale_y": resized_height / height}
    return np.asarray(canvas, dtype=np.int32)[None, ...], transform


def decode_output(output, transform):
    values = np.asarray(output)
    if values.shape != (1, 6, 56) or not np.isfinite(values).all():
        raise ValueError("invalid model output")
    detections = []
    for row in values[0]:
        score = float(row[55])
        if not 0 <= score <= 1:
            raise ValueError("invalid detection confidence")
        if score < SCORE_THRESHOLD:
            continue
        detection = {"score": score}
        for name, index in (("left_ankle", 15), ("right_ankle", 16)):
            y, x, confidence = map(float, row[index * 3:index * 3 + 3])
            if not 0 <= confidence <= 1:
                raise ValueError("invalid keypoint confidence")
            px = (x * transform["size"] - transform["left"]) / transform["scale_x"]
            py = (y * transform["size"] - transform["top"]) / transform["scale_y"]
            if not 0 <= px < transform["width"] or not 0 <= py < transform["height"]:
                # Padded/cropped feet cannot produce a usable floor ray. Retain a
                # required finite ankle struct, explicitly with zero confidence.
                confidence = 0.0
            detection[name] = {"x": min(max(px, 0), transform["width"] - 1),
                               "y": min(max(py, 0), transform["height"] - 1),
                               "confidence": confidence}
        detections.append(detection)
    return detections


class PoseDetector:
    def __init__(self, model_path, input_size=DEFAULT_SIZE):
        import onnxruntime as ort
        if input_size not in (256, 384, 512, 640):
            raise ValueError("unsupported input size")
        # Load the exact verified bytes, so a later path replacement cannot
        # change the model represented by the result's digest.
        model = Path(model_path).read_bytes()
        verify(model)
        options = ort.SessionOptions()
        options.intra_op_num_threads = 2
        options.inter_op_num_threads = 1
        self.session = ort.InferenceSession(model, options, providers=["CPUExecutionProvider"])
        inputs, outputs = self.session.get_inputs(), self.session.get_outputs()
        if (len(inputs) != 1 or inputs[0].name != "input" or inputs[0].type != "tensor(int32)"
                or len(outputs) != 1 or outputs[0].name != "output_0" or outputs[0].shape != [1, 6, 56]):
            raise ValueError("unexpected pinned model interface")
        self.input_size = input_size

    def infer(self, data):
        tensor, transform = prepare_image(data, self.input_size)
        output = self.session.run(["output_0"], {"input": tensor})[0]
        return {"model_id": METADATA["model_id"],
                "model_version": (METADATA["model_version"] + "+onnx." + METADATA["conversion_revision"]
                                  + ".adapter" + ADAPTER_VERSION + ".input" + str(self.input_size)),
                "model_sha256": METADATA["sha256"],
                "image_sha256": hashlib.sha256(data).hexdigest(),
                "image_width": transform["width"], "image_height": transform["height"],
                "detections": decode_output(output, transform)}

    def infer_path(self, path):
        with open(path, "rb") as source:
            return self.infer(source.read(MAX_IMAGE_BYTES + 1))


def serve(detector, source, target):
    while True:
        line = source.readline(MAX_REQUEST_BYTES + 1)
        if not line:
            break
        if len(line) > MAX_REQUEST_BYTES:
            raise ValueError("request byte bound")
        request = json.loads(line)
        if (not isinstance(request, dict) or set(request) != {"image_path"}
                or not isinstance(request["image_path"], str) or not request["image_path"]):
            raise ValueError("expected image_path only")
        result = detector.infer_path(request["image_path"])
        target.write(json.dumps(result, allow_nan=False, separators=(",", ":")) + "\n")
        target.flush()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--image")
    mode.add_argument("--serve", action="store_true")
    parser.add_argument("--input-size", type=int, choices=(256, 384, 512, 640), default=DEFAULT_SIZE)
    args = parser.parse_args()
    detector = PoseDetector(args.model, args.input_size)
    if args.serve:
        serve(detector, sys.stdin, sys.stdout)
    else:
        print(json.dumps(detector.infer_path(args.image), allow_nan=False, separators=(",", ":")))


if __name__ == "__main__":
    main()
