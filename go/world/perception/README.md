# Image pose inference

This adapter runs MoveNet MultiPose Lightning on actual RGB image bytes using
ONNX Runtime CPU. It returns anonymous per-image ankle observations for the Go
floor projector. It receives no scene object IDs, actor positions, calibration,
or simulator truth. It does not perform tracking or assign persistent IDs.

## Run locally

From the private repository root:

```sh
python3.12 -m venv .local/perception-venv
.local/perception-venv/bin/python -m pip install -r perception/requirements.txt
.local/perception-venv/bin/python perception/fetch_model.py \
  --output .local/perception-models/movenet-multipose-lightning.onnx
.local/perception-venv/bin/python perception/pose.py \
  --model .local/perception-models/movenet-multipose-lightning.onnx \
  --image frame.png
```

Use `--serve` instead of `--image` to keep the model loaded. Write one JSON line
`{"image_path":"frame.png"}` per request. Each response has exactly:

```json
{"model_id":"google/movenet/multipose/lightning","model_version":"1+onnx.<revision>.adapter1.input384","model_sha256":"<sha256>","image_sha256":"<sha256>","image_width":640,"image_height":480,"detections":[{"score":0.8,"left_ankle":{"x":160.0,"y":300.0,"confidence":0.7},"right_ankle":{"x":180.0,"y":300.0,"confidence":0.6}}]}
```

Coordinates refer to the original encoded image, with a top-left pixel origin.
The adapter retains low ankle confidences; the projector decides whether they
support a floor position. A predicted ankle in letterbox padding or outside the
image receives zero confidence and a clamped display coordinate. No detection
returns an empty array. It does not synthesize missing joints from a bounding box.

Inference uses RGB, centred black letterboxing, Pillow bilinear resizing and an
int32 tensor. Default input is 384×384; `--input-size` also permits 256, 512 and
640. The response model version records the selected input size and adapter
version. Instance score threshold is 0.2, the model supports at most six people,
and both ankles remain independently scored. EXIF rotation is not applied
because the supplied camera calibration describes the encoded pixel layout.

The model is loaded from its hash-verified bytes, never a mutable latest revision.
`model.json` pins its 19,049,169-byte ONNX conversion, source revision and SHA-256.
Weights are downloaded into ignored local storage and are not committed.
Inference needs no network. Unit tests run with:

```sh
.local/perception-venv/bin/python -m unittest perception.test_pose -v
```

## Linux container

Build locally, then run with image input mounted read-only. The image installs
the same three pinned runtime packages and fetches the same pinned model during
the build. Execution is offline; the model and code are already inside the image.

```sh
docker build -t wendy-world-pose:local perception
docker run --rm --network none --read-only --cap-drop ALL \
  --mount type=bind,src="$PWD/frames",dst=/frames,readonly \
  wendy-world-pose:local --image /frames/frame.png
```

The Dockerfile pins the existing Python 3.12 slim base by its manifest digest.
ONNX Runtime supports Linux CPU execution. Numeric parity across operating
systems is checked with a tolerance; bit-identical output is not assumed.

The first measured check processed 12 actual MuJoCo-rendered images from two
cameras. Both people were detected in the clear first and last views. During
crossing, person and foot detections were sometimes missing or below confidence
threshold. The black-image control returned no detections. The same 12 images
ran in an offline Linux arm64 container, with matching detection counts and a
maximum absolute numeric difference of 5.96e-8 against macOS arm64. Eight focused
adapter tests passed on both platforms. These are fixture results, not a general
person-detection accuracy evaluation.

## Model origin and limits

Google's [official model documentation](https://github.com/tensorflow/tfhub.dev/blob/master/assets/docs/google/models/movenet/multipose/lightning/1.md)
licenses the model under Apache 2.0 and defines its 17-joint output. The
[pinned Xenova ONNX conversion](https://huggingface.co/Xenova/movenet-multipose-lightning/tree/8506174df310cf154b97352afa98d1ea4b1e4b47)
also declares Apache 2.0. See [NOTICE.md](NOTICE.md) and the retained license.
No Ultralytics code or weights are used.

Google describes the model as aimed at movement and fitness imagery, particularly
webcam distances. Accuracy on our rendered mannequins is measured separately;
neither the model nor these fixtures establishes general office-person accuracy.
The model can miss occluded people and predict uncertain feet. Floor projection
must retain those limitations, original image provenance and calibration.
