# Fixture calibration, 2026-09-27

All nine new simulator fixtures passed with deterministic reference solutions and independent checks. These runs made no LLM calls. They validate setup, grading and cleanup, and do not count as agent success results.

| Task | Latest check | Seconds | Cleanup |
| --- | --- | ---: | --- |
| `create-go2-simulator` | passed | 59.5 | passed |
| `create-g1-simulator` | passed | 66.2 | passed |
| `debug-otel-logs` | passed | 30.2 | passed |
| `debug-otel-metrics` | passed | 28.7 | passed |
| `debug-otel-traces` | passed | 29.3 | passed |
| `debug-go2-ros2` | passed | 71.6 | passed |
| `repair-go2-camera` | passed | 68.4 | passed |
| `debug-g1-ros2` | passed | 72.6 | passed |
| `repair-g1-camera` | passed | 69.8 | passed |

The first Go2 ROS2 check failed because the grader called `python` in an image that only supplies `python3`. The probe was corrected, the old failure retained, and a fresh VM passed the rerun. All ten attempts cleaned up successfully.

The robot creation checks require the named Go2 or G1 runtime, isolated DDS, fresh odometry, correct frames and 12 or 29 distinct joints. ROS2 repair checks seed a missing-topic fault in a real consumer and require fresh data after repair. Camera checks disable the real virtual sensor, then require fresh RGB frames that change when the verifier moves an obstacle.

The OTEL checks export real protobuf logs, metrics and correlated spans into the Wendy collector. They seed a computation error or latency fault, apply a reference repair, and verify new challenge requests. Agent trials additionally require returned tool evidence that the agent queried the requested signal.

The CLI was built from isolated base `8f43d71409cd624c9e3b325bd821257aa072f1be` with token accounting and `WENDY_CONFIG_DIR` support. Its SHA-256 is `3142f99cf467224bf26205804d503e40ba2dad6de17ada5598270d51db143847`. The ARM64 agent SHA-256 is `2a0c558af633041feb67d2d32dbdeb598162e15771fc74da79e6514f614e98a1`. Each case used a fresh private VM store; image and build caches were shared.

Earlier deterministic checks passed deployment, startup repair, app update, start/stop, Compose creation and generic simulator creation. The [initial deployment pilot](pilot-2026-09-27.md) records the six actual LLM deployment attempts.

Physical installation, OS OTA, physical ROS2 diagnosis, physical camera testing, audio and peripheral I/O still require their lab adapters and targets. They remain unconfigured in the catalog. The supplied-SD-card erase authorization is recorded in [fixture setup](fixtures.md); no physical storage was flashed during these runs.

Raw transcripts and verifier records remain in the local result directories listed in the [machine-readable calibration report](fixture-calibration-2026-09-27.json).
