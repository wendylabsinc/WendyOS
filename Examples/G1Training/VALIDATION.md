# Source import validation — 2026-09-15

The 28 Python source/test files were copied from the local RTX 5090 deployment
and its contact-audit dependency. SHA256 checks verified the downloaded bytes.
The original and packaged hashes are in `source-provenance.json`.

## Completed checks

| Check | Result |
| --- | --- |
| Local regression suite, Python 3.10 / PyTorch 2.10.0 / MuJoCo 3.12.0 | 36 passed |
| Same packaged regression suite on the 5090 host, PyTorch 2.8.0+cu128 / MuJoCo 3.12.0 | 36 passed |
| Trainer `--help`, both environments | Exit 0; configurable data/cache roots present |
| Recorder `--help`, both environments | Exit 0 |
| Staged whitespace check | Passed |

The suite covers reference-bank hash/order and held-out-split enforcement,
sensor packing, recurrent migration and sequence parity, episode memory resets,
reward calculations, release timing and correction-controller behavior. An
initial packaging pass found a missing `release_audit.py` dependency in a
regression test; that source module was included before the passing runs.

The 5090 checks ran in an isolated temporary directory with
`CUDA_VISIBLE_DEVICES` empty. They did not start training, render an episode,
alter the running deployment or command a robot. The imported package has not
been subjected to a new end-to-end CUDA training run or full-task qualification.
The datasets, pretrained checkpoint and NVIDIA runtime remain external inputs.

## Runnable observation app — 2026-09-15

Deployed `sh.wendy.examples.g1-training` version `0.2.0` to the local arm64
`g1-sim` VM using the PR CLI `pr1984-44be244bc` and beta agent
`pr1984-498f8b895`. Simulator source digest:
`sha256:f81723d66c4e5b8ba9563075c89bf396c2f8fa10cdd4dd67ec43c8d86ad558ed`.
Both the managed simulator and this app reported `RUNNING`.

- The local suite passed **43 tests**, including seven new observation tests
  for stale reception, old source timestamps, simulator identity, invalid data,
  padded BGR images, stale camera responses and the grasp-policy boundary.
- `tests/verify_simulator_app.py --seconds 5` passed against the deployed app.
  Two `/healthz` responses were HTTP 200, with 250 new joint-state messages,
  1,000 IMU messages, 250 odometry messages and 75 RGB/CameraInfo pairs between
  them. Latest source ages were 21.37, 7.81, 21.37 and 47.06 ms, respectively.
- `/camera.jpg` returned a valid 17,822-byte JPEG from the received 640 x 360
  `rgb8` ROS image. The browser rendered the camera, the live stream counters,
  measured robot state and all 29 named joint positions.
- A separate app status sample showed lidar reception at 10 Hz and the latched
  simulation epoch. The app publishes no robot commands.
- Wendy reported an 8.467-second build (4 rebuilt steps), 10 reused device image
  layers and 8.2 MB sent in 0.5 seconds. The Docker Layer Optimizer fallback
  measured 9.407 seconds for the successful detached deployment. Its markers
  left that total unclassified; observer overhead and individual deployment
  phases were not measured. HTTP and ROS readiness were verified afterward.

The first manifest used host discovery and was rejected before building; the
final manifest uses the managed simulator's required `discoveryScope: app`,
domain 0 and host networking. The agent supplies the loopback DDS environment.

This validates app upload, browser rendering and actual ROS observation delivery.
The container does not contain a training dataset or checkpoint and does not run
the 43-joint grasp policy. The separate 5090 training workload was not changed.
