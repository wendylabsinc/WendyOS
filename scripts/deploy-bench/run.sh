#!/usr/bin/env bash
# Repeatable `wendy run` deploy benchmark for WDY-3210. See README.md.
set -euo pipefail

usage() {
	echo "usage: $0 <device> <big-layer|apt-layer|tiny-layers|one-line> [runs=3]" >&2
	exit 2
}
[ $# -ge 2 ] || usage
device=$1
scenario=$2
runs=${3:-3}
here=$(cd "$(dirname "$0")" && pwd)
src="$here/apps/$scenario"
[ -d "$src" ] || usage

# Work on a copy: the scenarios rewrite inputs between runs.
work=$(mktemp -d "${TMPDIR:-/tmp}/wendy-bench-$scenario.XXXXXX")
trap 'rm -rf "$work"' EXIT
cp -R "$src/." "$work/"

mkdir -p "$here/results"
stamp=$(date +%Y%m%d-%H%M%S)
log="$here/results/$stamp-$scenario.log"
csv="$here/results/$stamp-$scenario.csv"
echo "run,wall_s,summary" >"$csv"

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
extra_pkgs=(jq tree file less nano)

for i in $(seq 1 "$runs"); do
	case "$scenario" in
	big-layer | tiny-layers) printf '%s-%s\n' "$stamp" "$i" >"$work/seed" ;;
	apt-layer) printf '%s\n' "${extra_pkgs[*]:0:$((i - 1))}" >"$work/extra-packages.txt" ;;
	one-line) printf '# run %s-%s\n' "$stamp" "$i" >>"$work/app.py" ;;
	esac

	echo "=== $scenario run $i/$runs ===" | tee -a "$log"
	start=$(now)
	# --chunking force: a silent registry fallback would make the numbers meaningless.
	(cd "$work" && WENDY_TIMING=1 wendy run --device "$device" --detach --chunking force --yes) 2>&1 | tee -a "$log"
	end=$(now)
	wall=$(perl -e "printf '%.2f', $end - $start")
	summary=$(grep -E 'Sent [0-9]+ chunk|already on device' "$log" | tail -1 | tr ',' ';' || true)
	echo "$i,$wall,$summary" >>"$csv"

	# Agent phase logs ("RunContainer phase timings", "Wrote layer", "Prepared image").
	wendy device logs --device "$device" --json --tail 400 >>"$log.agent.jsonl" 2>/dev/null &
	logs_pid=$!
	sleep 3
	kill "$logs_pid" 2>/dev/null || true
done

echo "results: $csv"
