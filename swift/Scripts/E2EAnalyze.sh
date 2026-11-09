#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_OUTPUT_DIR="${WENDY_E2E_OUTPUT_DIR:-$SWIFT_DIR/../Build/e2e}"

OUTPUT_DIR="$DEFAULT_OUTPUT_DIR"
OPEN_REPORT="true"
STAGE="all"
RUN_PREFIX="${WENDY_E2E_ANALYZE_RUN_ID:-}"
DIFF=""

usage() {
  cat <<EOF
Usage: $(basename "$0") [--output-dir DIR] [--run-id ID] [--stage STAGE] [--diff RANGE] [--open|--no-open]

Analyze Swift E2E attempts found in an output directory.

Stages:
  aggregate  Aggregate attempts into matching run directories.
  review     Review existing run results.
  report     Render existing run HTML reports.
  all        Aggregate attempts, review runs, and render reports; default.

Options:
  --output-dir DIR  Directory containing Swift E2E attempt directories;
                    defaults to $DEFAULT_OUTPUT_DIR.
  --run-id ID       Analyze attempts matching this run ID prefix;
                    defaults to today's local run, or WENDY_E2E_RUN_ID/GITHUB_RUN_ID.
  --stage STAGE     aggregate, review, report, or all.
  --diff RANGE      Git diff range to pass to review stage.
  --open            Open the newest generated report when supported; default.
  --no-open         Do not open a report.
  --help            Show this help message.
EOF
}

expand_local_path() {
  local path="$1"
  case "$path" in
    '~')
      printf "%s" "${HOME:?}"
      ;;
    '~/'*)
      printf "%s/%s" "${HOME:?}" "${path#~/}"
      ;;
    *)
      printf "%s" "$path"
      ;;
  esac
}

absolute_dir_path() {
  local path
  path="$(expand_local_path "$1")"
  mkdir -p "$path"
  (cd "$path" && pwd)
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output-dir)
      OUTPUT_DIR="$2"
      shift 2
      ;;
    --run-id)
      RUN_PREFIX="$2"
      shift 2
      ;;
    --stage)
      STAGE="$2"
      shift 2
      ;;
    --diff)
      DIFF="$2"
      shift 2
      ;;
    --open)
      OPEN_REPORT="true"
      shift
      ;;
    --no-open)
      OPEN_REPORT="false"
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 64
      ;;
  esac
done

case "$STAGE" in
  aggregate|review|report|all)
    ;;
  *)
    echo "ERROR: --stage must be aggregate, review, report, or all." >&2
    exit 64
    ;;
esac

default_run_prefix() {
  if [[ -n "${WENDY_E2E_RUN_ID:-}" ]]; then
    local value="${WENDY_E2E_RUN_ID}"
    local without_attempt="${value%.*}"
    printf "%s" "${without_attempt%.*}"
    return
  fi

  if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
    printf "swift-e2e-tests.gh%s" "${GITHUB_RUN_ID}"
    return
  fi

  printf "swift-e2e-tests.local0000"
}

OUTPUT_DIR="$(absolute_dir_path "$OUTPUT_DIR")"
RUN_PREFIX="${RUN_PREFIX:-$(default_run_prefix)}"
RUN_PREFIX="${RUN_PREFIX%.}"

is_attempt_dir() {
  local dir="$1"
  local base="${dir##*/}"
  [[ -d "$dir" ]] || return 1
  [[ "$base" == "$RUN_PREFIX".* ]] || return 1
  [[ "$base" =~ \.[0-9][0-9][0-9][0-9]$ ]] || return 1
  [[ -f "$dir/attempt.json" ]]
}

is_run_dir() {
  local dir="$1"
  [[ -d "$dir" ]] || return 1
  [[ ! -f "$dir/attempt.json" ]]
}

run_dir_for_attempt() {
  local run_id="$1"
  local run_base run_name
  run_base="${run_id%.*}"
  run_name="${run_base%.*}"
  printf '%s/%s\n' "$OUTPUT_DIR" "$run_name"
}

load_attempt_dirs() {
  find "$OUTPUT_DIR" -mindepth 1 -maxdepth 1 -type d | sort | while IFS= read -r dir; do
    if is_attempt_dir "$dir"; then
      printf '%s\n' "$dir"
    fi
  done
}

load_run_dirs() {
  if [[ ${#ATTEMPT_DIRS[@]} -gt 0 ]]; then
    for attempt_dir in "${ATTEMPT_DIRS[@]}"; do
      run_dir_for_attempt "${attempt_dir##*/}"
    done | sort -u
    return
  fi

  find "$OUTPUT_DIR" -mindepth 1 -maxdepth 1 -type d | sort | while IFS= read -r dir; do
    [[ "${dir##*/}" == "$RUN_PREFIX" ]] || continue
    if is_run_dir "$dir"; then
      printf '%s\n' "$dir"
    fi
  done
}

mapfile -t ATTEMPT_DIRS < <(load_attempt_dirs)
mapfile -t RUN_DIRS < <(load_run_dirs)

if [[ "$STAGE" == "aggregate" || "$STAGE" == "all" ]]; then
  if [[ ${#ATTEMPT_DIRS[@]} -eq 0 ]]; then
    echo "ERROR: no Swift E2E attempt directories found in $OUTPUT_DIR." >&2
    exit 64
  fi
fi

if [[ "$STAGE" == "review" || "$STAGE" == "report" ]]; then
  if [[ ${#RUN_DIRS[@]} -eq 0 ]]; then
    echo "ERROR: no Swift E2E run directories found in $OUTPUT_DIR." >&2
    exit 64
  fi
fi

echo "==> Analyzing Swift E2E runs"
echo "    Stage:      $STAGE"
echo "    Run ID:     $RUN_PREFIX"
echo "    Output dir: $OUTPUT_DIR"
if [[ -n "$DIFF" ]]; then
  echo "    Diff:       $DIFF"
fi
for attempt_dir in "${ATTEMPT_DIRS[@]}"; do
  echo "    Attempt:    $attempt_dir"
done
for run_dir in "${RUN_DIRS[@]}"; do
  echo "    Run:        $run_dir"
done

status=0
declare -A failed_reviews=()

if [[ "$STAGE" == "aggregate" || "$STAGE" == "all" ]]; then
  bash "$SCRIPT_DIR/E2EAggregate.sh" \
    --output-dir "$OUTPUT_DIR" \
    "${ATTEMPT_DIRS[@]}" || status=$?
fi

if [[ "$STAGE" == "review" || "$STAGE" == "all" ]]; then
  review_args=()
  if [[ -n "$DIFF" ]]; then
    review_args+=(--diff "$DIFF")
  fi
  for run_dir in "${RUN_DIRS[@]}"; do
    bash "$SCRIPT_DIR/E2EReview.sh" --run-dir "$run_dir" "${review_args[@]}" || {
      step_status=$?
      failed_reviews["$run_dir"]=1
      [[ $status -eq 0 ]] && status=$step_status
    }
  done
fi

if [[ "$STAGE" == "report" || "$STAGE" == "all" ]]; then
  for run_dir in "${RUN_DIRS[@]}"; do
    (
      # Otherwise the report reads a review that never finished as a clean result.
      if [[ -n "${failed_reviews[$run_dir]:-}" ]]; then
        export WENDY_E2E_REVIEW_OUTCOME=failure
      fi
      bash "$SCRIPT_DIR/E2EReport.sh" --run-dir "$run_dir"
    ) || {
      step_status=$?
      [[ $status -eq 0 ]] && status=$step_status
    }
  done
fi

if [[ "$STAGE" == "report" || "$STAGE" == "all" ]]; then
  latest_report=""
  latest_mtime=0
  for run_dir in "${RUN_DIRS[@]}"; do
    report_path="$run_dir/index.html"
    [[ -f "$report_path" ]] || continue
    mtime="$(stat -f %m "$report_path" 2>/dev/null || stat -c %Y "$report_path" 2>/dev/null || echo 0)"
    if [[ "$mtime" -ge "$latest_mtime" ]]; then
      latest_mtime="$mtime"
      latest_report="$report_path"
    fi
  done

  if [[ -n "$latest_report" ]]; then
    if [[ "$OPEN_REPORT" == "true" && "$(uname -s)" == "Darwin" ]]; then
      open "$latest_report" || echo "HTML report: $latest_report"
    else
      echo "HTML report: $latest_report"
    fi
  else
    echo "HTML report not found in analyzed run directories." >&2
    [[ $status -eq 0 ]] && status=1
  fi
fi

exit "$status"
