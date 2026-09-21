# G1 reference-conditioned training

Train recurrent PPO corrections for the G1 can pick-and-place task. This example
contains the native-MuJoCo training pipeline deployed on the local RTX 5090 on
2026-09-15, plus its policy, sensor contract, reward functions and regression tests.

The nominal motion comes from a recorded 43-joint reference. The learned policy
corrects 15 command slots: waist yaw (12), right arm (29–35), and right Dex3 hand
(36–42). These are indices in the recorded simulation command array.

Physics runs at 1,000 Hz in CPU MuJoCo with float64 state. The reference controller
runs at 40 Hz, and MuJoCo renders RGB-D and visible-object masks at 20 Hz through
OpenGL. CUDA runs the frozen visual/joint encoders, recurrent memory, actor,
critic and PPO updates. The local configuration uses 20 simulation processes
and one CUDA learner.

## Relationship to the Wendy G1 simulator

The [G1 virtual robot](../../go/simulator/g1/README.md) provides a 29-joint VM
profile with native/ROS interfaces. This training example uses recorded
43-joint models with Dex3 hands and matching reference files. Its checkpoint
requires those references; it cannot be loaded directly as the virtual robot's
locomotion policy. The example has no robot command publisher.

## Upload the observation app to a simulator

This directory is also a runnable Wendy app. Its container receives live ROS 2
RGB, camera calibration, joint states, IMU, odometry, lidar scans and simulation
epoch. A browser view at port 8891 shows the received camera image, message
counts, rates, freshness and joint positions. It reports the grasp policy's
missing inputs explicitly; it does not run the GPU trainer or grasp inference.
The Docker context includes only this small observation runtime.

Use a CLI and VM agent built from PR #1984, following the
[G1 virtual robot setup](../../go/simulator/g1/README.md). With `g1-sim` running,
run the matching CLI from this directory (`wendy-beta` on the development Mac):

```sh
wendy-beta run --device vm:g1-sim
```

The explicit device selects the simulator directly. In the interactive picker,
switch to the Simulators tab and select the same VM. Wendy forwards the HTTP
port to [localhost:8891](http://127.0.0.1:8891) on the Mac. The simulator's own
control panel remains at [localhost:8890](http://127.0.0.1:8890).

```sh
curl --fail http://127.0.0.1:8891/healthz
curl --fail http://127.0.0.1:8891/api/status
python3 tests/verify_simulator_app.py --vm g1-sim --seconds 5
```

`/healthz` returns 200 only when the local G1 simulator reports healthy and the
app has fresh RGB, CameraInfo, JointState, Imu and Odometry messages. Both receipt
age and message-header age are checked. Its TCP readiness probe only establishes
that the HTTP server is reachable. Lidar is optional, and the epoch is latched.
`/camera.jpg` returns 503 when the camera data is stale. All observations come
from ROS subscriptions; the simulator HTTP endpoint supplies identity and mode.

The app uses host networking with `discoveryScope: app`. Wendy configures
CycloneDDS on the VM's loopback interface and domain 0, matching this simulator's
DDS isolation. It has no command
publishers. The training commands below run separately on a GPU workstation with
the matching dataset and checkpoint.

## Install

Use Python 3.10 on Linux with an NVIDIA GPU and an EGL-capable NVIDIA driver.
The versions below match the source deployment, including CUDA 12.8 PyTorch.

```sh
cd Examples/G1Training
python3.10 -m venv .venv
.venv/bin/pip install torch==2.8.0 --index-url https://download.pytorch.org/whl/cu128
.venv/bin/pip install -r requirements-dev.txt
```

The source import does not include datasets, model binaries, checkpoints, cached
features or videos. Obtain the existing reference bank and parent checkpoint
from the training operator. The required inputs are:

| Input | Required content |
| --- | --- |
| Parent checkpoint | Model, optimizer, normalization and training contract, including reference order and cache-manifest SHA256 |
| Data root | `dataset/<episode>/episode.json`, with the original metadata bytes |
| Cache root | The exact `manifest.json` bound to the parent checkpoint |
| Each reference directory | `model.mjb`, `rollout.npz`, `result.json`, and `verification.json` |

The loader admits exactly the 768 training references. It verifies the manifest
and episode-metadata hashes and the parent's reference order. The 106 validation
and 126 test references are outside training. Dataset and cache roots are
configurable, but absolute trajectory paths in the original metadata and parent
contract must still resolve (for example, by mounting the corpus at those paths).
Rewriting the metadata breaks its hash binding.

## Start a new training run

Run from this example directory. Replace the paths with the existing artifact
locations; the output directory must be new. This reproduces the local run's
20-worker, 128-step, 32-step recurrent-chunk configuration.

```sh
MUJOCO_GL=egl OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1 MKL_NUM_THREADS=1 \
  .venv/bin/python reference_rl/cpu_run.py \
  --root outputs/new-run \
  --parent /path/to/parent-update-002525.pt \
  --data-root /path/to/visual-bc \
  --cache-root /path/to/feature-cache \
  --mesh-mode process --worlds 20 --physics-workers 20 \
  --updates 1000000 --steps 128 --sequence-length 32 --hours 8 \
  --can-position-half-range-m 0.06 \
  --can-yaw-range-rad 3.141592653589793 \
  --can-table-edge-margin-m 0.005 --randomization-seed 20260915 \
  --reset-value-optimizer-for-reward-change
```

Only one supervisor should own an output directory. `--resume-in-place` resumes
its newest atomic checkpoint. For a deadline that survives supervisor restarts,
pass the same `--end-time-epoch` on every invocation; `--hours` is relative to
the current process's training start. The deployed machine's systemd service
and host-specific launcher are managed separately from this example.

The actor and GRU are retained from a recurrent parent. A changed physics or
reward contract resets the critic and optimizer; reward changes require the
explicit reset flag. A non-recurrent parent additionally requires a trained BC
`--memory-checkpoint`. The trainer performs a complete nominal reference check
before starting a new run. Training reference coverage, rollout outcomes,
losses, throughput and contact diagnostics are written to `status.json`,
`metrics.jsonl`, `completed-episodes.json`, and checkpoint contracts.

The imported trainer also contains NCCL and authenticated HTTP collective
support. This example's instructions and import validation cover a single local
learner; distributed deployment and coordinator provisioning are separate.

## Observation and reward contract

The correction actor receives encoded RGB, depth, visible can mask and measured
joint features, plus current reference targets/velocities and causal correction
state. It uses 316 features and per-world recurrent memory. Simulator object
state is used for reward and audit calculations. The actor does not receive
the simulator's object pose. Memory resets at episode boundaries; PPO chunks
do not cross those boundaries. Bootstrap inference does not advance memory.

The source run rewards tracking, opposed grip, three-finger grip, eligible palm
contact and sustained lift above 8 cm. Contact with the top of the can during
the grip phase receives a penalty proportional to contact time. The imported
simulation reward has no force ceiling. These reward parameters are recorded
in checkpoint contracts; they are not hardware control limits.

## Record a frozen checkpoint

Copy the checkpoint to a stable path before recording. Use its matching
reference and a stated randomization seed:

```sh
MUJOCO_GL=egl .venv/bin/python reference_rl/cpu_policy_video.py \
  --root outputs/review-001 --checkpoint /path/to/frozen.pt \
  --reference /path/to/reference --randomization-seed 20260915
```

The recorder writes a video, state/action arrays and a report. A training
checkpoint, a successful import or a partial lift does not establish complete
pick-and-place success or physical robot qualification.

## Development and provenance

```sh
.venv/bin/python -m pytest -q
.venv/bin/python reference_rl/cpu_run.py --help
.venv/bin/python reference_rl/cpu_policy_video.py --help
```

[source-provenance.json](source-provenance.json) records the original deployed
file hashes and the imported hashes. Packaging changes replace two machine-local
dataset constants with CLI arguments, bundle the contact audit, include that
audit in new checkpoint source hashes, and retain only the two shared artifact
helpers from the original exporter. Policy, reward and physics logic are
preserved. [VALIDATION.md](VALIDATION.md) records checks of this import.
