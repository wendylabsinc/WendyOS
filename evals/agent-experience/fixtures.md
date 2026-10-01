# User workflow coverage and lab fixtures

Each task measures Codex, Claude Code and Wendy chat separately. Physical cloud
devices, direct hardware, generic VMs, Go2 VMs and G1 VMs are distinct targets.
Keep the model and effort configured in each product, and record both.

Sixteen tasks have built-in executable setup, verification and cleanup, including
the OTEL cases described below and separate Go2/G1 ROS and camera fault cases.
Seven cases need external lab fixture code for physical installation, OTA,
physical ROS 2 consumer repair, camera capture/repair, audio and peripheral I/O.
Those cases have prompts and acceptance contracts.
Registering a capability alone does not enable them. Each target must supply
real commands for all three phases before the runner will launch an LLM.

| Task | Fixture required | Independent evidence |
| --- | --- | --- |
| `install-physical` | Supplied disposable Pi 5 SD card or explicitly named equivalent; stable media identity; first-boot address; controllable boot/cabling | Written artifact, actual boot into expected WendyOS, device identity, nonce-bearing smoke app |
| `ota-update` | Dedicated target at a pinned baseline, exact target artifact, repeatable baseline restoration | Changed boot/slot/build, committed update status, original transport restored, retained app and random persistent data |
| `debug-otel-logs` | Instrumented failing app and reproducible request | Queried app-scoped OTEL error with request ID, repaired source, fresh successful workload |
| `debug-otel-metrics` | Instrumented workload with a seeded throughput, error-rate or latency fault | Queried series with units and interval, independently measured recovery under load, instrumentation retained |
| `debug-otel-traces` | Instrumented request path with a failing or slow operation | Queried parent/child spans and trace ID, correct causal diagnosis, healthy fresh request trace |
| `debug-ros2` | Real DDS publisher/consumer and seeded topic, QoS, domain, frame or clock fault | Repaired consumer processes fresh correctly typed data with expected frames and rate |
| `camera-capture` | Named camera and random visual stimulus or changing virtual scene | Decoded frames with dimensions, fresh timestamps, correct camera identity and matching pixels |
| `camera-repair` | Camera app with a reproduced access, device-selection, format or sensor-enable fault | Repair in the actual capture path; independent fresh stimulus capture |
| `audio-loopback` | Named playback/capture endpoints, controlled physical or virtual loopback, known fault | Fresh challenge waveform recovered with sample-rate, correlation, latency, level and clipping checks |
| `peripheral-io` | Identified serial, GPIO, I2C or other disposable test fixture with a known fault | Fresh challenge/response or independently measured electrical transition |

Handing over a disposable SD card authorizes wiping that card. The trial still
needs its exact identity, which the prepare phase must resolve before any model
runs. Never infer the target from “first removable disk.” Capture drive serial,
size, device node and board model. Protect other drives with the lab runner's
permissions. This suite does not erase any media merely because it is visible.

Human card transfer, recovery-button and power operations need explicit records.
For a fully unattended installation score, arrange those operations before the
timed attempt or provide a controlled USB/power fixture. A person helping after
the initial prompt makes it an assisted run, retained separately. A successful
installation plan without a write and first boot cannot pass.

For OTA, an agent binary update is insufficient. A target already on the desired
build is an invalid starting condition. Restore the baseline between attempts,
and pin artifact identity even when nightly builds share a version string. The
verifier must check the update engine's committed verdict and persistent data
after reconnecting. The reset procedure belongs to the lab fixture, outside the
timed task. Do not run repeated updates on an arbitrary enrolled device.

Go2 and G1 use separate ROS fixtures with their own message layouts. The current
managed simulators publish wall-clock stamps and no `/clock`. A generic VM with
an invented topic does not count as a hardware-specific simulator. The G1 runtime
does not implement factory audio services. A separate virtual audio loopback
tests the audio application path; it must not be labeled G1 audio parity.

## Register a fixture

The following is a configuration shape, not an implemented Pi flashing script.
Replace the paths and target details with an independently validated lab adapter.

```json
{
  "kind": "physical",
  "device": "192.0.2.10:50051",
  "capabilities": ["disposable-install-media"],
  "fixtures": {
    "install-physical": {
      "disposable_target": {
        "board": "raspberry-pi-5",
        "media_serial": "EXPLICITLY_HANDED_OVER_CARD_ID"
      },
      "prompt_context": "The supplied Pi 5 card, first-boot address and artifact are identified in installation-brief.json. This specific medium may be completely erased.",
      "prepare": ["/absolute/lab/fixture", "prepare", "install-physical", "--target-config", "{target_config}", "--workspace", "{workspace}", "--evidence", "{evidence_dir}", "--run-id", "{run_id}"],
      "verify": ["/absolute/lab/fixture", "verify", "install-physical", "--target-config", "{target_config}", "--evidence", "{evidence_dir}", "--events", "{agent_events}", "--run-id", "{run_id}"],
      "cleanup": ["/absolute/lab/fixture", "cleanup", "install-physical", "--target-config", "{target_config}", "--evidence", "{evidence_dir}", "--run-id", "{run_id}"]
    }
  }
}
```

Commands are argument arrays, executed without a shell. The runner substitutes
the listed variables plus `wendy`, `device`, `app_id`, `vm_name`, `port`, `root`
and `here`. Keep adapter code and authoritative evidence outside the LLM's
editable workspace. The configuration, command arguments and outputs are retained,
so put credential references there, never secret values.

Prepare must validate resource identity and starting state before creating the
workload or writing the initial brief. If the baseline or stimulus is unavailable,
exit nonzero so the result becomes `setup_error`, with no model cost. Verify must
check the full acceptance contract in `tasks.json` and exit nonzero on any missing
evidence. It must not repair the task on the model's behalf. For telemetry tasks,
inspect actual tool results in `agent_events`; mentioning a signal in the final
answer is insufficient. Cleanup must be idempotent, handle partial setup, and
touch only explicitly owned resources. Cleanup failure stops further attempts.

An adapter that cannot independently grade its task remains unconfigured. Do not
use `true`, an empty script, a model's self-assessment, or successful CLI exit as
a replacement verifier. Exercise each adapter with known-good and known-bad
outcomes before collecting paid trials.
