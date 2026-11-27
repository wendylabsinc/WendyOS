#!/usr/bin/env bash
set -euo pipefail

DEFAULT_REPO="${WENDY_E2E_CI_REPO:-wendylabsinc/WendyOS}"
DEFAULT_WORKFLOW="${WENDY_E2E_CI_WORKFLOW:-swift-e2e-tests.yml}"

REPO="$DEFAULT_REPO"
WORKFLOW="$DEFAULT_WORKFLOW"
REF=""
TESTS=""
DIFF_BASE_REF=""
WATCH="false"
DRY_RUN="false"

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Trigger the Swift E2E GitHub Actions workflow_dispatch run.

Options:
  --ref REF            Workflow ref to run, matching the Actions UI "Run
                       workflow from" selector. Defaults to the current git
                       branch, or main when not in a branch.
  --tests FILTERS      Comma-separated SwiftPM test filters; empty runs the
                       default WendyE2ETests suite.
  --diff-base-ref REF  Optional base for aggregate comparison and AI diff review; empty = full.
  --repo OWNER/REPO    GitHub repository; defaults to $DEFAULT_REPO.
  --workflow FILE      Workflow file or ID; defaults to $DEFAULT_WORKFLOW.
  --watch              Watch the newest matching workflow_dispatch run after
                       triggering it.
  --dry-run            Print the gh command without running it.
  --help               Show this help message.

Examples:
  $(basename "$0") --ref kb.swift-e2e-tests --diff-base-ref main
  $(basename "$0") --ref main
  $(basename "$0") --ref kb.swift-e2e-tests --tests 'SomeSuite.testName'
EOF
}

default_ref() {
  local branch
  branch="$(git branch --show-current 2>/dev/null || true)"
  if [[ -n "$branch" ]]; then
    printf '%s' "$branch"
  else
    printf 'main'
  fi
}

require_value() {
  local option="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    echo "ERROR: $option requires a value." >&2
    usage >&2
    exit 64
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref)
      require_value "$1" "${2:-}"
      REF="$2"
      shift 2
      ;;
    --tests)
      require_value "$1" "${2:-}"
      TESTS="$2"
      shift 2
      ;;
    --diff-base-ref)
      require_value "$1" "${2:-}"
      DIFF_BASE_REF="$2"
      shift 2
      ;;
    --repo)
      require_value "$1" "${2:-}"
      REPO="$2"
      shift 2
      ;;
    --workflow)
      require_value "$1" "${2:-}"
      WORKFLOW="$2"
      shift 2
      ;;
    --watch)
      WATCH="true"
      shift
      ;;
    --dry-run)
      DRY_RUN="true"
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

REF="${REF:-$(default_ref)}"

if ! command -v gh >/dev/null 2>&1; then
  echo "ERROR: gh is required to trigger the Swift E2E workflow." >&2
  exit 69
fi

command_args=(
  workflow run "$WORKFLOW"
  --repo "$REPO"
  --ref "$REF"
)
if [[ -n "$TESTS" ]]; then
  command_args+=(-f "tests=$TESTS")
fi
if [[ -n "$DIFF_BASE_REF" ]]; then
  command_args+=(-f "diff_base_ref=$DIFF_BASE_REF")
fi

printf '==> Triggering Swift E2E CI\n'
printf '    Repo:          %s\n' "$REPO"
printf '    Workflow:      %s\n' "$WORKFLOW"
printf '    Ref:           %s\n' "$REF"
printf '    Tests:         %s\n' "${TESTS:-<default>}"
printf '    Review:        %s\n' "$([[ -n "$DIFF_BASE_REF" ]] && printf 'diff against %s' "$DIFF_BASE_REF" || printf 'full')"
printf '    Command:       gh'
printf ' %q' "${command_args[@]}"
printf '\n'

if [[ "$DRY_RUN" == "true" ]]; then
  exit 0
fi

triggered_after="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
gh "${command_args[@]}"

run_list_args=(
  run list
  --repo "$REPO"
  --workflow "$WORKFLOW"
  --event workflow_dispatch
  --limit 20
  --json databaseId,url,status,conclusion,headBranch,headSha,createdAt
)
run_filter='[.[] | select(.createdAt >= env.WENDY_E2E_TRIGGERED_AFTER)] | sort_by(.createdAt) | reverse'

if [[ "$WATCH" != "true" ]]; then
  echo "==> Matching workflow_dispatch runs created by this trigger"
  WENDY_E2E_TRIGGERED_AFTER="$triggered_after" gh "${run_list_args[@]}" --jq "$run_filter"
  exit 0
fi

run_id=""
for _ in {1..30}; do
  run_id="$(WENDY_E2E_TRIGGERED_AFTER="$triggered_after" gh "${run_list_args[@]}" --jq "${run_filter} | .[0].databaseId // empty")"
  if [[ -n "$run_id" ]]; then
    break
  fi
  sleep 2
done

if [[ -z "$run_id" ]]; then
  echo "ERROR: triggered run was not found yet; check GitHub Actions." >&2
  exit 1
fi

gh run watch "$run_id" --repo "$REPO"
