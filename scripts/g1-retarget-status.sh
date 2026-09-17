#!/usr/bin/env bash
set -euo pipefail

SWEEP_ROOT="/workspace/g1-object-retarget-sweep-20260917"
NETWORK_VOLUME_ID="4ujarub7y6"
WATCH_SECONDS=""
RAW_JSON=0

usage() {
  echo "Usage: bash scripts/g1-retarget-status.sh [--watch [seconds]] [--json]"
}

while (($#)); do
  case "$1" in
    --watch)
      if [[ ${2:-} =~ ^[0-9]+([.][0-9]+)?$ ]]; then
        WATCH_SECONDS="$2"
        shift 2
      else
        WATCH_SECONDS="30"
        shift
      fi
      ;;
    --json)
      RAW_JSON=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

command -v runpodctl >/dev/null || {
  echo "g1-retarget-status: runpodctl is not installed" >&2
  exit 1
}
command -v jq >/dev/null || {
  echo "g1-retarget-status: jq is not installed" >&2
  exit 1
}

discover_pod() {
  local pods pod_id pod volume_id runtime ip port key name cost
  pods="$(runpodctl pod list)"
  while IFS= read -r pod_id; do
    [[ -n "$pod_id" ]] || continue
    pod="$(runpodctl pod get "$pod_id" 2>/dev/null)" || continue
    volume_id="$(jq -r '.networkVolumeId // empty' <<<"$pod")"
    runtime="$(jq -r '.runtimeStatus // empty' <<<"$pod")"
    [[ "$volume_id" == "$NETWORK_VOLUME_ID" && "$runtime" == "running" ]] || continue
    ip="$(jq -r '.ssh.ip // empty' <<<"$pod")"
    port="$(jq -r '.ssh.port // empty' <<<"$pod")"
    key="$(jq -r '.ssh.ssh_key.path // empty' <<<"$pod")"
    name="$(jq -r '.name // "unnamed"' <<<"$pod")"
    cost="$(jq -r '.costPerHr // 0' <<<"$pod")"
    [[ -n "$ip" && -n "$port" && -n "$key" ]] || continue
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$pod_id" "$name" "$cost" "$ip" "$port" "$key"
    return 0
  done < <(jq -r '.[] | select(.runtimeStatus == "running") | .id' <<<"$pods")
  return 1
}

read_once() {
  local discovered pod_id pod_name pod_cost pod_ip pod_port pod_key
  discovered="$(discover_pod)" || {
    echo "g1-retarget-status: no running pod can read network volume $NETWORK_VOLUME_ID" >&2
    return 1
  }
  IFS=$'\t' read -r pod_id pod_name pod_cost pod_ip pod_port pod_key <<<"$discovered"

  if [[ "$RAW_JSON" -eq 0 ]]; then
    printf 'G1 object-retarget status | %s\n' "$(date '+%Y-%m-%d %H:%M:%S %Z')"
    printf 'Runpod: %s (%s) | $%.2f/hr | read-only\n' "$pod_name" "$pod_id" "$pod_cost"
  fi

  ssh \
    -i "$pod_key" \
    -p "$pod_port" \
    -o BatchMode=yes \
    -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=accept-new \
    "root@$pod_ip" \
    python - "$SWEEP_ROOT" "$RAW_JSON" <<'PY'
import json
import subprocess
import sys
from pathlib import Path

root = Path(sys.argv[1])
raw = sys.argv[2] == "1"


def load(path):
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None


def rate(value):
    return value.get("rate") if isinstance(value, dict) else value


def evaluation(path):
    data = load(path)
    if not data:
        return None
    conditionals = data.get("conditionals", {})
    return {
        "path": str(path),
        "episodes": data.get("episodes"),
        "successes": data.get("successes"),
        "success_rate": data.get("success_rate"),
        "catastrophic": data.get("catastrophic_episodes"),
        "score": data.get("score"),
        "opposed_given_contact": rate(conditionals.get("opposed_given_contact")),
        "zone_given_lift8": rate(conditionals.get("zone_given_lift8")),
        "release_given_landing": rate(conditionals.get("release_given_landing")),
        "success_given_lift8": rate(conditionals.get("success_given_lift8")),
    }


process = subprocess.run(
    ["pgrep", "-af", "run_zone_arm|zone_evaluate|gpu_run|run_retarget_screen"],
    text=True,
    capture_output=True,
)

screen = {}
for path in sorted((root / "screen").glob("gain-*.json")):
    if path.name.endswith("-launch.json"):
        continue
    item = evaluation(path)
    if item:
        screen[path.stem.removeprefix("gain-")] = item

confirm = {}
for path in sorted((root / "confirm-4cm").glob("gain-*.json")):
    item = evaluation(path)
    if item:
        confirm[path.stem.removeprefix("gain-")] = item

training = {}
training_root = root / "training-sweep"
if training_root.exists():
    for arm in sorted(path for path in training_root.iterdir() if path.is_dir()):
        evaluations = {}
        for path in sorted(arm.glob("evaluation-u*.json")):
            item = evaluation(path)
            if item:
                evaluations[path.stem.removeprefix("evaluation-")] = item
        training[arm.name] = {
            "status": load(arm / "status.json"),
            "completion": load(arm / "completion.json"),
            "evaluations": evaluations,
        }

data = {
    "root": str(root),
    "exists": root.exists(),
    "selected": load(root / "selected-policy.json"),
    "selected_markdown": str(root / "SELECTED_POLICY.md"),
    "validation": load(root / "retarget-validation.json"),
    "active_processes": [line for line in process.stdout.splitlines() if line.strip()],
    "screen_2cm": screen,
    "confirm_4cm": confirm,
    "training": training,
}

if raw:
    print(json.dumps(data, indent=2))
    raise SystemExit(0)


def percent(value):
    return "n/a" if value is None else f"{100 * float(value):.1f}%"


def result_line(name, item):
    success = f"{item.get('successes')}/{item.get('episodes')} ({percent(item.get('success_rate'))})"
    return (
        f"{name:<24} {success:<20} "
        f"opp {percent(item.get('opposed_given_contact')):<7} "
        f"zone {percent(item.get('zone_given_lift8')):<7} "
        f"cat {item.get('catastrophic', 'n/a')}"
    )


print(f"Artifacts: {data['root']}")
selected = data.get("selected") or {}
if selected:
    retarget = selected.get("object_retarget") or {}
    print(
        "Selected: untouched Stage 2 + object retarget "
        f"gain={retarget.get('gain')} damping={retarget.get('damping')} "
        f"smoothing={retarget.get('smoothing')}"
    )
    print(f"Checkpoint SHA: {selected.get('selected_checkpoint_sha256')}")
else:
    print("Selected: selection manifest not found")

processes = data.get("active_processes") or []
print(f"Active sweep processes: {len(processes)}")
for line in processes:
    print(f"  {line}")

if screen:
    print("\nHeld-out gain screen (±2 cm, ±1 rad)")
    for gain, item in screen.items():
        print(result_line(f"gain {gain}", item))

if confirm:
    print("\nWider confirmation (±4 cm, ±1 rad)")
    for gain, item in confirm.items():
        print(result_line(f"gain {gain}", item))

if training:
    print("\nPPO continuations")
    for arm, details in training.items():
        state = (details.get("status") or {}).get("state", "unknown")
        print(f"{arm}: {state}")
        for update, item in (details.get("evaluations") or {}).items():
            print("  " + result_line(update, item))

print(f"\nReport: {data['selected_markdown']}")
PY
}

while true; do
  if [[ -n "$WATCH_SECONDS" && -t 1 && "$RAW_JSON" -eq 0 ]]; then
    printf '\033[2J\033[H'
  fi
  read_once
  [[ -n "$WATCH_SECONDS" ]] || break
  sleep "$WATCH_SECONDS"
done
