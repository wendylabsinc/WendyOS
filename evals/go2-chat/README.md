# Go2 chat simulation evals

This suite runs Wendy's production chat engine, device-control prompt, tool
schemas, approval flow, and command receipt handling against an in-memory Go2.
It tests the actions the chat actually requests and the resulting simulated
state. No physical robot, Wendy device connection, cloud account, or API key is
used. Movement speeds are not changed by this suite.

## Run locally

From the repository root, run the deterministic regression suite:

```sh
python3 evals/go2-chat/run.py
```

For the LLM eval, start local Ollama and download the same model family used by
Go2 chat. The runner does not download models or start an inference server:

```sh
ollama pull qwen3-vl:4b-instruct
python3 evals/go2-chat/run.py --mode model
```

Use `--cases find_miss,find_model_phrasing,find_no_motion` for the CI smoke
selection. Use `--repetitions 3` to measure variability. Every repetition must
pass; attempts are not retried until a passing result appears. `--model` selects
another installed Ollama model, and `--expected-digest` can require its exact
manifest digest. The default model is the instruct variant, not the thinking
variant. The model receives the production tool catalog and a 2,048-token
response limit. The eval fixes temperature to 0 and seed to 42 in its HTTP
transport; model/runtime versions can still change results.

The runner prints a temporary report directory outside the checkout. Set
`--output /tmp/my-go2-eval` to choose a new directory. Reports include the source
revision and dirty status, catalog hash, model/runtime metadata, each user turn,
assistant messages, tool arguments/results, simulated state, and grading
failures. A failed case returns a nonzero exit status. Missing models, unknown
case IDs, timeouts, and model API errors are failures too.

The runner defaults to a 120-second budget per model case. Use
`--timeout-seconds 600` on slower CPUs. CI sets the same 600-second limit through
`WENDY_GO2_EVAL_TIMEOUT_SECONDS`. Each case permits at most 16 model rounds. These
limits bound the test; they do not modify the robot's motion budgets.

## What is checked

Cases live in
[`cases.json`](../../go/internal/cli/chat/testdata/go2-eval/cases.json).
The 22 cases cover finding a chair after a current-view miss, alternate wording,
finding and approaching a person, explicit stationary restrictions, navigation
with and without prior alignment, forward/turn sequences, Stop, an offline
robot, blocked floor, invalid camera pose, lost or malformed receipts, default
active localization, and explicitly passive localization.

Find, navigation, and localization requests permit bounded movement by default. Explicit
no-motion requests must remain stationary. Graders check completion, distance
and heading, action order, the requested object, evidence identities, dispatch
counts, and receipt polling without replaying movement. Targeted response
checks reject common unsupported success claims; they are not a semantic judge
of arbitrary prose. Grader tests deliberately inject
wrong outcomes to verify that doing nothing, duplicating movement, or claiming
unearned success cannot pass.

There are two distinct modes:

- **Scripted regression:** prescribed model tool decisions test the production
  engine and simulator contracts. This is deterministic plumbing coverage,
  not evidence that an LLM understood a command.
- **Model eval:** the real local model chooses tools through the production
  HTTP provider. It uses the same simulated states and graders. All selected
  cases are required to pass; failures are retained for investigation.

The simulator supplies synthetic camera detections and controller outcomes.
This tests language, planning, prerequisites, tool use, and error handling. It
does not test image recognition, LiDAR geometry, Unitree actuator deadbands,
gait physics, or real-world safety. Those need their existing geometry,
controller, simulation, and hardware tests. In particular, a passing chat eval
does not qualify a change to the Go2's minimum speeds.

## CI and isolation

[`go2-chat-evals.yml`](../../.github/workflows/go2-chat-evals.yml) runs the full
scripted suite and a real-model smoke selection on affected pull requests.
Weekly and manual runs execute every model case. Model failures fail the job;
there is no `continue-on-error`. Traces are uploaded even on failure.

CI downloads Ollama 0.32.1 and the model before evaluation, then launches the
model and test executable together in a Docker container with `--network none`,
no device mounts, no capabilities, and a read-only filesystem. Only temporary
files and the report directory are writable. The model manifest digest is
checked before execution and the runtime image digest is recorded. No hardware
credentials or device identities are passed into that container.

Local evals allow only an explicit literal loopback HTTP endpoint. The model
transport disables proxies and redirects and pins dialing to that address.
The simulator never constructs the real tool executor or executes MCP handlers.
The separate catalog exporter registers production schemas without executing
them. Any unsupported tool call fails instead of falling through to real tools.

## Add a regression

Add a case with a unique ID, initial simulated state, user turns, scripted
decisions for the deterministic mode, and expected state/actions. Specify the
requested query and forbidden actions where relevant. Keep grading independent
of the scripted sequence, and add a negative grader test if introducing a new
expectation. Do not copy real device identities, transcripts, images, weights,
credentials, or runtime state into fixtures. Run both modes for the affected
case before treating it as model coverage.
