"""Trusted operator replay, only while Scout is in supported bootstrap mode."""

import datetime, json, os, pathlib, sys, time
from common import STATE, save
from functional import WORKFLOWS
from local import Local
from probes import Probes

if os.environ.get("SCOUT_BOOTSTRAP") != "1":
    raise SystemExit(
        "Redeploy with SCOUT_BOOTSTRAP=1 so model sessions cannot race this replay"
    )
if (
    len(sys.argv) != 3
    or sys.argv[1] not in WORKFLOWS
    or sys.argv[2] not in {"past", "stable"}
):
    raise SystemExit(
        "Usage: reproduce.py <restart_persistence|reject_extra_args|logs_output> <past|stable>"
    )
local = Local(os.environ.get("WENDY_AGENT_SOCKET", ""))
local.verify_development()
p = Probes(json.loads((STATE / "release-pins.json").read_text()), local.scratch, local)
p.run("info", "stable", None)
p.deadline = time.monotonic() + 90
run = (
    STATE
    / "runs"
    / datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
)
run.mkdir(parents=True)
result = p.replay(sys.argv[1], sys.argv[2], "nightly", run)
save(run / "operator-result.json", result)
save(
    run / "summary.json",
    {
        "operator": True,
        "proof": False,
        "summary": "Trusted operator replay; inspect operator-result.json",
        "experiments": [],
    },
)
print(json.dumps({"run": run.name, **result}, indent=2))
