# Project experiment recipes

`experiment_plan` expands `wendy.experiment.v1` into a normal saved workflow.
It returns the recipe/workflow SHA-256, combination and step counts, declared
step-timeout sum, and `execution_started: false`. This is structural validation,
not proof of scientific validity, source compatibility, readiness or execution.
The `training` catalog includes planning. The smaller `workflow` catalog keeps
only execution/observation tools. Execution retains `job_submit` authorization.

Example for an existing project evaluator (adapt its exact argv to your project):

```json
{
  "schema": "wendy.experiment.v1",
  "parameters": {"condition": ["control", "changed"], "seed": ["11", "13"]},
  "before_steps": [
    {"name": "preflight", "argv": ["python3", "preflight.py"], "timeout_seconds": 30}
  ],
  "workflow": {
    "schema": "wendy.workflow.v1",
    "job": {
      "name": "paired-evaluation-001",
      "working_directory": "/absolute/owned/project",
      "steps": [{
        "name": "eval-{{condition}}-{{seed}}",
        "argv": ["python3", "evaluate.py", "--condition", "{{condition}}", "--seed", "{{seed}}", "--output", "{step_dir}/result.json"],
        "timeout_seconds": 60
      }]
    },
    "wait_seconds": 300,
    "artifact_destination": "/absolute/local/reports",
    "artifact_files": ["status.json", "eval-{{condition}}-{{seed}}/result.json"],
    "report_fields": {"eval-{{condition}}-{{seed}}/result.json": ["/successes", "/episodes"]}
  }
}
```

The four evaluations run sequentially, after the preflight. `after_steps` may
declare a project-owned comparison step; the planner does not invent a grader,
reward, optimizer, environment adapter, restart policy or budget. For scored
work, retain the project's hash-pinned `source_manifest` or `source_hashes` in
the workflow job and use independent outcome checks. Recipe-authoring, setup,
retries and missing-evidence handling belong in cold benchmark accounting.

Axes sort by parameter name; values retain their declared order. Every axis
must be used. Values are nonempty unique strings, at most 160 bytes, with no
braces/NUL. There are at most 4 axes, 32 combinations and 32 total steps including
before/after steps. Split larger studies into separately named explicit jobs;
no silent truncation, budget reduction, parallel scheduling or best-seed choice.

Only repeated step names/argv/metrics paths and selected artifact names/report
pointers accept `{{parameter}}`. Job identity, source contracts, working directory,
local destination and before/after steps stay fixed. `{job_dir}`/`{step_dir}`
remain runtime path tokens. Values are literal argv strings: there is no shell
expansion or executable template code. Names must remain unique after expansion.
The normal 64-file/16-MiB collection and 8-report/32-scalar projection bounds still
apply. Static artifact names are included once. Conflicting projections fail.

The recipe and destination must be absolute local paths. The parent directory
must exist; the planner refuses to replace an existing file. It never connects,
stages sources, installs dependencies or starts training. Submit the resulting
workflow separately only within the user's execution scope.
