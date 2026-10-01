# Change detection

A fixed-camera Wendy app that detects changed regions, compares aligned before
and after crops, and tracks persistence, growth, and movement. The dashboard
shows the reference, current frame, compensated residual, region masks, class
scores, and liquid-candidate events.

The default demo runs without a camera or model download. It uses an **untrained
rule baseline**, whose scores are not probabilities. An optional Siamese model
and training command use the same crops, masks, and temporal features. No trained
leak detector or real-world accuracy claim is included.

## Run locally

Use Python 3.11 or 3.12.

```sh
cd ~/git/wendy/wendyos/Examples/ChangeDetection
python3.11 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python app.py
```

Open <http://127.0.0.1:8000>. Choose a scene, pause to inspect its crops, or reset
to restart that scene. Synthetic time advances two seconds per frame at two
frames per second. Growing liquid should become a candidate around 16 simulated
seconds. Shadow, lighting, new object, camera movement, and no-change scenes
exercise the other paths.

```sh
# USB/V4L2 camera. Capture a dry, clear view before starting.
python app.py --source 0 --scene-id pump-room-camera-1

# Recorded video. Timestamps come from its frame rate.
python app.py --source /path/to/recording.mp4 --fps 2 --scene-id recording-1

# OpenCV-compatible stream.
python app.py --source rtsp://camera.example/stream --scene-id camera-2

# Change the local port.
python app.py --port 8001
```

The first frame becomes the reference. For a camera, **Reset reference** uses
the next captured frame and clears tracks and events. Only reset when that view
is an acceptable baseline. A spill already present in the reference cannot be
detected as a new change. Video completion or capture failure stops processing,
displays the reason, and leaves the last result available for inspection.

Local serving binds to `127.0.0.1`. Use `--host 0.0.0.0` to allow other machines
on a trusted network. This example has shared controls and no authentication.
Pause, scene selection, and reference resets affect every connected viewer.

## Run on WendyOS

```sh
cd ~/git/wendy/wendyos/Examples/ChangeDetection
wendy run --device <device-name>

# Capture a device camera instead of running the synthetic demo.
wendy run --device <device-name> --env SOURCE=0 --env SCENE_ID=pump-room-camera-1
```

`wendy.json` grants camera access and exposes HTTP on port 8000. The Stagefile
installs the CPU dependencies and starts `app.py` on all interfaces. It follows
the other camera examples by running as root for V4L2 device access. The default
demo needs no GPU. `SOURCE`, `SCENE_ID`, `HOST`, `PORT`, and `CHECKPOINT` can also
be supplied as environment variables.

The default image contains the rule baseline. To deploy a trained checkpoint,
change the Stagefile's pip requirements to `requirements-training.txt`, add a
local copy entry with `paths: [models]` and `dest: /app/models/`, put the checkpoint there, and set
`CHECKPOINT=/app/models/change-classifier.pt`. Training dependencies are separate
so the default sample stays small. Stock CPU inference is implemented; Jetson
GPU wheels and CUDA execution are not configured in this example.

## Detection and temporal decisions

```text
Reference + current frame
    -> compensate median brightness shift
    -> residual, threshold, connected regions
    -> associate regions across frames
    -> aligned before/after crops + region mask + measured features
    -> rule baseline OR shared-encoder Siamese classifier
    -> persistence and motion rules
    -> liquid candidate for review
```

- The reference stays frozen. A stationary change cannot disappear into an
  adapting background.
- A per-channel median offset removes additive global brightness changes.
  Phase correlation checks for camera translation. A shift above three pixels
  or a compensated change covering over 55% of the frame suppresses candidates
  and asks for camera restoration or a new reference.
- Connected regions above 90 pixels are matched by overlap and centroid
  distance. Processing uses at most 640 pixels of width and 16 regions.
- Each region has `age_seconds`, relative area `growth_rate` per second,
  `movement_speed` in frame diagonals per second,
  `brightness_change_global` relative to 255, `area_fraction`, signed
  `mean_delta` relative to 255, `mask_fill`, and relative `texture_change`.
  Growth and speed use exponential smoothing over the last observations.
- A liquid candidate starts after a liquid score of at least 0.70, at least
  eight seconds of age, motion below 0.015 frame diagonals per second, and
  three qualifying observations. It stays active while the region is present,
  even if growth stops. A missing region clears its active alert and streak;
  unmatched tracks expire after five seconds. Events remain in the log.

Defaults are in `change_detection/pipeline.py`. These are sample thresholds,
not parameters validated for a particular floor, camera, or installation.

The rule baseline can suggest `liquid`, `shadow`, or `new_object`, and otherwise
abstains as `unknown`. Global lighting is a frame-level status because its
compensated mask should contain no local region. The full model vocabulary is:

```text
liquid, lighting, shadow, person, moved_object, new_object,
surface_damage, smoke, debris, unknown
```

## Collect labeled examples

1. Give each camera/session a meaningful `--scene-id`.
2. Pause, select a region, and choose its **Human label**.
3. Select **Export crop**. Extract the ZIP into a dataset directory.
4. Add a `split` field of `train` or `val` to its `sample.json` object and append
   that object as one line in `manifest.jsonl` at the dataset root.

Each ZIP contains aligned `before.png`, `after.png`, `mask.png`, and
`sample.json`. Image paths in the JSON are relative to the dataset root after
extraction. The app exports the measured features with the images. Preserve
their units and avoid making up growth or motion values from a single pair.
The export marks synthetic examples with `synthetic: true`.

A manifest row has this shape:

```json
{"before":"sample-1/before.png","after":"sample-1/after.png","mask":"sample-1/mask.png","label":"liquid","scene_id":"camera-1-session-1","split":"train","features":{"age_seconds":24,"growth_rate":0.02,"movement_speed":0.0002,"brightness_change_global":0.01,"area_fraction":0.03,"mean_delta":-0.12,"mask_fill":0.65,"texture_change":-0.15}}
```

Keep an entire camera/session in one split. For cross-camera evaluation, keep
all sessions from the same camera in one split as well. The trainer rejects
identical `scene_id` values across splits, but cannot detect two names that
refer to the same camera. Both splits are required. Include confounding changes
and `unknown` examples. Synthetic scenes exercise the software; they do not
establish performance on real leaks.

## Train the Siamese classifier

```sh
pip install -r requirements-training.txt
python -m change_detection.train data/manifest.jsonl \
  --epochs 10 --output models/change-classifier.pt
python app.py --source 0 --checkpoint models/change-classifier.pt
```

The trainer loads [TorchVision's pretrained ResNet-18](https://docs.pytorch.org/vision/main/models/generated/torchvision.models.resnet18.html)
and freezes its weights and BatchNorm statistics. Its first use downloads the
ImageNet weights. Both crops pass through the same encoder. The head receives
both embeddings, their absolute difference, an encoded change mask, and the
eight normalized temporal features. Training and inference share the image
preprocessing and feature ordering. Crops are resized to 160 by 160, converted
to RGB, and normalized with ImageNet channel means and standard deviations.

The trainer prints loss, accuracy, and a confusion matrix for each split and
saves the checkpoint with the lowest validation loss. Confusion matrix rows
are true labels and columns are predicted labels in the vocabulary order above.
Use `--device cuda` or `--device mps` for training if your PyTorch installation
supports it. Runtime inference uses CPU. No checkpoint download happens during
inference; its encoder weights are included in the saved file.

Model scores are softmax probabilities, but are not calibrated. Predictions
below 0.50 maximum probability are labeled `unknown` without changing the
reported probabilities. Evaluate detection misses, false alerts per camera-day,
delay to first alert, and probability calibration on unseen cameras before
using the output operationally. Crop classification cannot recover a leak that
the preceding change detector missed.

## Limits

This sample assumes a fixed camera and a known acceptable reference. Additive
brightness compensation does not cover all exposure, white balance, sunlight,
or reflection changes. Small rotation and perspective changes may escape the
translation check. Static shadows, stains, reflective liquid, occlusion, region
merges, and region splits can confuse detection or track identity. The rule
baseline deliberately leaves a static dark patch unknown unless it has first
observed sufficient growth. It cannot recognize people, smoke, debris, or damage.

The capture backend determines supported cameras/codecs and buffering behavior.
A best-effort one-frame buffer request may be ignored by some cameras or RTSP
backends. There is no camera reconnect loop. Events and the last 120 samples
live in memory; restarting clears them. At the nominal two frames per second,
the demo advances at four times real time, but slower processing lowers that
playback speed.

## API and checks

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | 200 when the worker has no input error; 503 after an input failure or video end. |
| `GET /api/state` | Frames as data URLs, regions, features, scores, events, and history. |
| `POST /api/pause` | JSON `{"paused":true}` or `{"paused":false}`. |
| `POST /api/reset` | JSON `{}` to capture a new reference or restart the demo. |
| `POST /api/demo` | JSON `{"scenario":"shadow"}`, only in demo mode. |
| `POST /api/export` | JSON `{"id":1,"label":"liquid"}` to download the current region as a ZIP. |

```sh
python -m unittest discover -s tests -v
node --check static/app.js
```

The tests cover positive and negative scenes, stationary persistence, camera
motion, crop alignment, multiple tracks, expiry, timestamps, sampled video time,
HTTP controls, health failures, and ZIP exports. Installing training dependencies
also enables tests for a real gradient update, frozen BatchNorm statistics,
checkpoint loading, and split leakage. Model tests use random encoder weights
and make no network requests or accuracy claims.
