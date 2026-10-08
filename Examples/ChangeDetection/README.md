# Visual and audio change detection

A fixed-camera Wendy app that detects changed regions, compares aligned before
and after crops, and tracks persistence, growth, and movement. The dashboard
shows the reference, current frame, compensated residual, region masks, class
scores, and liquid-candidate events.

An optional audio monitor adds independent hopper and conveyor channels for
[WDY-3366](https://linear.app/wendylabsinc/issue/WDY-3366/simulation-suncor-oil-sands-dump-hopper-conveyor-metal-detection-and).
It compares PCM audio with a frozen normal-operation baseline, confirms
metal-impact and belt-rip **candidates**, retains evidence clips, and latches an
in-process conveyor-stop simulation. Synthetic audio, recorded WAVs, and live
microphone input share the same detector. Audio is off unless explicitly enabled.

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

## Hopper and conveyor audio

Run both synthetic microphone channels alongside a quiet camera scene:

```sh
python app.py --scenario empty --audio-source demo --scene-id suncor-demo
```

The audio panel shows a hopper/conveyor schematic with microphone placements,
waveform envelopes, spectra, anomaly histories, and the simulated conveyor state.
Select **Metal impact in hopper** or **Conveyor belt rip**. Each scenario starts
with normal operation, introduces a sound at five seconds, and confirms its
candidate at 5.5 or 5.75 seconds respectively. The confirmation then stops the
belt animation. Audio runs at real time; the original visual demo has its own
accelerated clock. Changing a visual scene does not reset audio.

Normal operation, an unrelated tone, silence, and clipping exercise the negative
paths. Silence and clipping are input-quality statuses; an unfamiliar sound is
`unknown`. None is interpreted as confirmed metal or a belt rip. Audio continues
after a simulated stop so that evidence remains available. **Restart audio
session** clears the stop, events and baseline. Selecting a different audio
scenario also restarts that session. Pause freezes synthetic/file playback;
live microphone samples are discarded while paused so resuming cannot replay
stale sound. Pause interrupts any partial confirmation.

Select an event to listen to its retained evidence. Choose a human label and
export a ZIP containing `audio.wav` and `sample.json`. Each clip includes up to
three seconds ending at confirmation. Metadata contains the predicted and human
labels, zone, source timestamps, UTC observation time, measured features, anomaly
score, confirmation duration, and simulated stop dispatch duration. The latest
30 events and clips are held in memory; restarting clears them. Confidence is
explicitly `null`: the rule score is not a calibrated probability.

### Recorded and live audio

```sh
# Stereo: left channel = hopper microphone, right channel = conveyor microphone.
python app.py --scenario empty --audio-source /path/to/recording.wav

# Mono: choose the physical microphone's zone.
python app.py --scenario empty --audio-source /path/to/belt.wav --audio-zone conveyor

# Live microphone capture, with an optional PortAudio device index or name.
pip install -r requirements.txt -r requirements-audio.txt
python -m sounddevice
python app.py --scenario empty --audio-source mic --audio-zone hopper --audio-device 0
```

WAV input supports uncompressed 16-bit PCM, mono or stereo, at 8–96 kHz. It uses
sample-count timestamps and stops at EOF, retaining the last result. An
incomplete final window is discarded. Microphones capture mono at 16 kHz using
[sounddevice/PortAudio](https://python-sounddevice.readthedocs.io/en/0.5.1/api/streams.html).
On Linux, install `libportaudio2`; the Wendy container includes it. The local
synthetic and WAV modes only need `requirements.txt` and do not open a microphone.
Microphone overflow, timeout, and device errors stop audio processing and make
`/health` return 503. There is no automatic reconnect. Restarting audio clears
its error and attempts processing again; an unplugged device may require an
application restart after reconnection. Video continues independently.

Start with at least three seconds of representative normal machine operation.
Recalibrate when the operating condition changes. The detector cannot recover
an anomaly that was incorporated into its normal baseline. Camera and audio
timestamps are independent; this example does not fuse or synchronize them.

### Audio detector and stop semantics

Each channel processes non-overlapping 250 ms windows. Short overlapping FFTs
measure high-frequency energy, tonality, flatness and spectral centroid. RMS
level is measured in dBFS, not calibrated acoustic dB SPL. The first three seconds
of valid sound establish a median and median absolute deviation for RMS level
and relative high-frequency energy. That baseline then stays frozen.

An anomaly score of six or more means one of those measurements deviated at
least six robust scale units. A minimum scale of 3 dB limits sensitivity to an
artificially stable reference. Increased high-band energy plus tonal ringing
in the hopper yields a metal-impact candidate. Increased broadband high-frequency
energy in the conveyor yields a belt-rip candidate. They require 0.5 and 0.75
seconds of consecutive evidence respectively. Gaps, pauses, and bad signal
quality break confirmation. A sustained candidate generates one event until
it clears for one second. These thresholds and signatures are sample rules,
not a trained acoustic classifier.

Either confirmed audio channel latches `SimulatedConveyor`. Events report
`stop_transport: "in_process_simulation"`. `stop_latency_ms` measures only the
in-process latch operation after confirmation. It excludes the capture window,
confirmation time, processing, network, PLC and mechanical stopping time.
`confirmation_seconds` and source timestamps are reported separately.

This implements the **two audio paths as a prototype** for WDY-3366. It does
not implement visual tramp-metal or visual belt-rip recognition, Modbus/OPC UA,
a real safety-rated emergency stop, or a hosted client showcase. The existing
liquid detector is unchanged. Machinery, rocks and other impacts can resemble
these synthetic signatures. Real recordings, held-out site evaluation and a
trained/calibrated classifier are needed before claiming detection performance.
Silence and capture failure report monitoring loss rather than issuing a
physical stop. No physical outputs are connected.

The audio exports are evidence for a future audio dataset. They are not inputs
to the image-based Siamese trainer below. Collect normal operation, confounding
sounds and faults across loads and microphone placements; split by recording
session/site when developing an audio model.

## Run on WendyOS

```sh
cd ~/git/wendy/wendyos/Examples/ChangeDetection
wendy run --device <device-name>

# Capture a device camera instead of running the synthetic demo.
wendy run --device <device-name> --env SOURCE=0 --env SCENE_ID=pump-room-camera-1

# Add synthetic audio, or use AUDIO_SOURCE=mic with AUDIO_ZONE=conveyor.
wendy run --device <device-name> --env AUDIO_SOURCE=demo --env AUDIO_SCENARIO=belt_rip
```

`wendy.json` grants camera and audio access and exposes HTTP on port 8000. The Stagefile
installs the CPU dependencies and starts `app.py` on all interfaces. It follows
the other camera examples by running as root for V4L2 device access. The default
demo needs no GPU. `SOURCE`, `SCENE_ID`, `HOST`, `PORT`, and `CHECKPOINT` can also
be supplied as environment variables.

Audio settings are `AUDIO_SOURCE`, `AUDIO_SCENARIO`, `AUDIO_ZONE`, and
`AUDIO_DEVICE`. The container includes microphone dependencies from
`requirements-audio.txt`; audio still defaults to off. No GPU or model download
is needed for the audio baseline.

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
| `GET /api/audio/clip?id=1` | Retained event evidence as a WAV for listening. |
| `POST /api/audio/pause` | JSON `{"paused":true}` or `{"paused":false}`; independent of video. |
| `POST /api/audio/reset` | Restart audio, recalibrate, clear evidence and the simulated stop. |
| `POST /api/audio/demo` | JSON `{"scenario":"belt_rip"}`; synthetic audio only. |
| `POST /api/audio/export` | JSON `{"id":1,"label":"belt_rip"}` for a labeled WAV + metadata ZIP. |

`/api/state` includes audio channels, measurements, events and simulated conveyor
state under `audio`. `/health` includes `audio_error` and returns 503 if either
enabled input worker has failed or reached EOF. Calibration and signal-quality
statuses live in `/api/state`; HTTP health does not certify detection readiness.

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

Audio tests cover both positive scenarios and negative controls, calibration,
temporal gaps and confirmation, frozen baselines, multiple sample rates, WAV
replay and EOF, microphone overflow, pause/reset, evidence export, worker failure,
and the latched simulated stop. Microphone tests use a fake capture device;
physical microphone access and deployment must be verified on the target device.
