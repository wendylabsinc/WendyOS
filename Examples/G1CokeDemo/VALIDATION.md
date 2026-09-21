# Validation

Checked on 16 September 2026 with the supplied pinned assets.

## Local checks

- The asset importer verified all 17 files, about 201 MiB. Binary assets remain ignored by Git.
- Python regressions passed, 30 tests. The ROS integration test skips outside a ROS environment.
- All 100 sealed inference comparisons passed at absolute and relative tolerance `1e-5`. Maximum action error was `1.0728836e-5`; maximum joint-target error was `1.1920929e-7` radians on macOS ARM64.
- The target-driven scene matched the supplied expert physics for 100 steps. Camera rendering uses separate MuJoCo data so it preserves the physics solver state.
- The complete 2,400-step expert replay lifted the can `0.3362592 m` and ended `0.0099454 m` horizontally from the marker.
- The live RGB camera produced valid frames. JavaScript syntax checks passed. Browser interaction and visual layout were not tested because no browser was available.

## ROS transport

The integration test passed in the WendyOS `g1-sim` VM using ROS 2 Humble and CycloneDDS. It exercised real trajectory delivery, joint and camera messages, depth bytes, camera calibration, IMU, odometry, transforms, simulation clock, late subscribers and the reset service.

The test uses temporary files and ROS domain 77 to keep its synthetic clock separate from the application on domain 0.

## Hardware-in-the-loop support

- The updated Python suite passes 52 tests; one ROS test skips outside the ROS
  environment. Four JavaScript tests cover command recovery and initial HIL
  controller selection without overwriting later choices.
- Local HTTP tests cover exact image transport, camera reuse, lost-reply
  retries, reset idempotency, old sessions, skipped/changed steps, checkpoint
  and joint-order checks, invalid targets, service restarts and partial
  inference failures.
- Lossless compressed requests pass the same CPU parity check. A decompressed
  size limit rejects oversized input, and delayed replies do not skip steps.
- Remote CPU targets match the existing scene-policy inference sequence over
  four steps, repeated after an episode reset, at `atol=1e-7`, `rtol=1e-6`.
- A separate CPU inference process and the real rendered MuJoCo scene
  completed a 40-step HIL run, representing one simulation second, with zero
  physical commands. This check ran directly on macOS without ROS.
- Jetson CUDA execution, the Jetson Stagefile build, and a live Wendy Cloud
  tunnel have not been validated. The discovered LAN device timed out and an
  Orin could not be identified among online cloud devices.
- Native CLI tests cover HIL project staging, invalid paths and configuration,
  tunnel transport and cancellation, health checks and incompatible run modes.
  `wendy run --hil` uses the existing Go build/deploy functions for both
  apps. No project launcher script is involved.
- Picker tests verify that `--hil` requires interactive selection, shows a
  single available device, preserves cancellation, and lets an explicit
  `--hil=DEVICE` bypass the picker.
- The browser controller changes have automated JavaScript checks. Visual
  inspection was unavailable because no browser was connected.

## Scope

The scene is the supplied fixed-pelvis, 43-joint attempt-000001 model. It retains the original waist constraints, desk layout and Dex3 hands. The checkpoint's golden trace belongs to attempt-000159. Golden parity verifies inference against that trace; it does not establish success in the new scene.

The learned controller uses the original 7,518-step timing. A preliminary run on the expert replay's 60-second retiming failed to lift the can, so that timing is reserved for expert replay. Policy observations come directly from copied scene state and are also published over ROS. Both built-in controllers send their targets through DDS before physics applies them.

Local development uses Python 3.12 and NumPy 2.2.6. The container uses Python 3.10 and NumPy 1.26.4 to match Humble's Python and message ABI. Both use PyTorch 2.7.1 and MuJoCo 3.12.0.
