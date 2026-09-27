# Robot simulator pilot, 2026-09-27

Codex and Wendy chat each passed Go2 and G1 creation without steering. Claude created a working G1 simulator but did not finish its verification turn. The operator stopped it after almost 16 minutes of repeated command errors. Its Go2 attempt did not run because final usage was unavailable. This is one attempt per tested agent/model/robot combination, not a reliability estimate.

| Agent | Robot | Result | Seconds | Total tokens | Estimated cost |
| --- | --- | --- | ---: | ---: | ---: |
| claude | G1 | interrupted | 954.7 | unknown | unknown |
| codex | G1 | passed | 227.2 | 1,244,791 | $4.08 |
| codex | Go2 | passed | 357.9 | 1,191,606 | $3.65 |
| wendy | G1 | passed | 110.5 | 233,887 | $1.16 |
| wendy | Go2 | passed | 115.1 | 223,016 | $1.12 |
| claude | Go2 | not run | | | |

The four completed attempts total $10.01 in estimated token cost. The interrupted attempt has incomplete usage, so the whole cohort cost is unknown. OpenAI estimates use the conservative rates recorded in the experiment manifest. These are budget estimates, not invoices or subscription charges. Cached input is counted once in total tokens.

The grader independently verified both robot identities, healthy managed runtimes, isolated DDS, fresh odometry and 12 Go2 or 29 G1 joints. It checked passive runtime status before connecting so it could not provision an unfinished runtime. The independent G1 check also passed after Claude was stopped, but the agent had not completed its turn. That row remains interrupted and unsuccessful. Cleanup passed for all five attempts; no attempt processes remained afterward.

Claude repeatedly packed `--device` and its value into one shell variable, then used the unquoted variable in commands that returned `unknown command "ros2"`. An independent argument-only check showed that zsh passes this variable as one argument, while bash splits it. This is a likely explanation for the retry loop; the transcript does not establish an intermittent robot failure. No correction was sent to the measured agent.

The robot task configuration allowed 1800 seconds. Progress updates incorrectly stated a 900-second limit. The operator ended this attempt early to limit further spending. Its elapsed time is an interrupted observation, not time to successful completion or a natural timeout.

Models were pinned to the configured choices: Codex used requested model `gpt-6-astra` with host xhigh effort; Claude Code reported `claude-opus-4-8` with medium effort; Wendy chat reported `gpt-6-astra`, API default reasoning and the saved 4096-token response limit. Codex JSON events did not expose an observed model name, so its requested model is recorded separately. Client versions were Codex 0.157.1 and Claude Code 2.1.272.

Each attempt used a fresh workspace, private Wendy configuration and VM store, one initial prompt and no human follow-up or approvals. The compatible ARM64 agent binary was identified in that prompt; any required update counted toward task time. Shared image/build caches were warm. This measures configured product experiences, with different models, settings and instructions.

The candidate base was `8f43d71409cd624c9e3b325bd821257aa072f1be` with token accounting and `WENDY_CONFIG_DIR` support. Its binary hash and detailed measurements are in the [JSON report](robot-pilot-2026-09-27.json). Local raw evidence is retained under `results/20260927-094326-93ff50/`.

Earlier trials are retained separately because permissions, grading and state isolation changed. They include a valid Wendy Go2 pass, Codex cache-permission failures and an inconclusive G1 result affected by an unrelated VM disappearing. An earlier Claude attempt changed host localhost identity pins and was stopped; the original pins were restored. These outcomes were not overwritten by this cohort. Several interrupted attempts have unknown final usage, so further paid trials have stopped pending budget reconciliation.

See the [deployment pilot](pilot-2026-09-27.md) for six successful cloud/VM app deployments and the [fixture calibration report](fixture-calibration-2026-09-27.md) for real OTEL, ROS2 and camera checks with no measured agent calls. Physical installation, OS OTA, physical peripheral and audio cases still need concrete lab adapters and targets.
