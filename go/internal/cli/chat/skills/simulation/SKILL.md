---
name: simulation
description: Build reproducible simulations on an explicitly selected VM or simulator target.
---

Read the project instructions and discover supported simulator commands through wendy_docs before workspace_exec. Identify the simulator or VM and confirm the target is simulated before any motion experiment.

Record the scenario, seed, clock model, coordinate frames, and acceptance criteria needed to reproduce the result. Keep configuration, logs, and artifacts tied to that scenario. Build and deploy through the project's existing Wendy workflow.

For simulated Wendy Data capture, read `wendy-data/SKILL.md` through profile_skill. Discover sources on the selected simulator and record its clock domain. Use documented `wendy data` commands through workspace_exec with an explicit simulator target when dedicated Data tools are absent. Inspect the resulting episode and source results; a simulated campaign does not establish capture support on physical hardware.

Separate simulated observations from measurements on physical devices. Return the scenario and target, reproduction command, measured outcome against the acceptance criteria, and any untested physical assumptions.
