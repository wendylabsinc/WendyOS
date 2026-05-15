#!/usr/bin/env bash
set -euo pipefail

WORKFLOW="${WORKFLOW:-swift-e2e-tests.yml}"
ARTIFACT_PATTERN="${ARTIFACT_PATTERN:-wendy-e2e-*}"
OUTPUT_ROOT="${OUTPUT_ROOT:-swift/Build/e2e-ci-analysis}"
BRANCH="${BRANCH:-}"
REPO="${REPO:-}"

usage() {
  cat <<EOF
Usage: $(basename "$0") [--branch NAME] [--repo OWNER/REPO] [--output-root DIR]

Fetch Swift E2E artifacts for AI analysis from the latest completed CI run for
the current branch/PR.

Environment:
  WORKFLOW          Workflow name or file; defaults to swift-e2e-tests.yml.
  ARTIFACT_PATTERN  Artifact glob; defaults to wendy-e2e-*.
  BRANCH            Branch override.
  REPO              GitHub repository override (OWNER/REPO).
  OUTPUT_ROOT       Output root; defaults to swift/Build/e2e-ci-analysis.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --branch)
      BRANCH="$2"
      shift 2
      ;;
    --repo)
      REPO="$2"
      shift 2
      ;;
    --output-root)
      OUTPUT_ROOT="$2"
      shift 2
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

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if [[ -z "$BRANCH" ]]; then
  BRANCH="$(git branch --show-current)"
fi
if [[ -z "$BRANCH" ]]; then
  BRANCH="$(git rev-parse --abbrev-ref HEAD)"
fi
if [[ -z "$BRANCH" || "$BRANCH" == "HEAD" ]]; then
  echo "ERROR: could not determine current branch; pass --branch." >&2
  exit 64
fi

if [[ -z "$REPO" ]]; then
  REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
fi

pr_number="$(gh pr view "$BRANCH" --repo "$REPO" --json number --jq .number 2>/dev/null || true)"
pr_url="$(gh pr view "$BRANCH" --repo "$REPO" --json url --jq .url 2>/dev/null || true)"

run_id="$(gh run list \
  --repo "$REPO" \
  --workflow "$WORKFLOW" \
  --branch "$BRANCH" \
  --status completed \
  --limit 1 \
  --json databaseId \
  --jq '.[0].databaseId // ""')"

if [[ -z "$run_id" ]]; then
  echo "ERROR: no completed $WORKFLOW run found for $REPO branch $BRANCH." >&2
  if [[ -n "$pr_number" ]]; then
    echo "PR: #$pr_number $pr_url" >&2
    echo "Recent PR checks:" >&2
    gh pr checks "$pr_number" --repo "$REPO" || true
  fi
  echo "Recent workflow runs:" >&2
  gh run list --repo "$REPO" --workflow "$WORKFLOW" --status completed --limit 20 || true
  exit 1
fi

run_url="$(gh run view "$run_id" --repo "$REPO" --json url --jq .url)"
run_conclusion="$(gh run view "$run_id" --repo "$REPO" --json conclusion --jq '.conclusion // ""')"
run_created_at="$(gh run view "$run_id" --repo "$REPO" --json createdAt --jq '.createdAt // ""')"

out="$OUTPUT_ROOT/run-$run_id"
rm -rf "$out"
mkdir -p "$out/artifacts"

gh run download "$run_id" \
  --repo "$REPO" \
  --pattern "$ARTIFACT_PATTERN" \
  --dir "$out/artifacts"

cat > "$out/metadata.json" <<EOF
{
  "repository": "$REPO",
  "branch": "$BRANCH",
  "pullRequest": ${pr_number:-null},
  "pullRequestURL": "${pr_url:-}",
  "workflow": "$WORKFLOW",
  "runID": "$run_id",
  "runURL": "$run_url",
  "conclusion": "$run_conclusion",
  "createdAt": "$run_created_at",
  "artifactPattern": "$ARTIFACT_PATTERN",
  "outputDirectory": "$out"
}
EOF

cat <<EOF
repository=$REPO
branch=$BRANCH
pull_request=${pr_number:-}
run_id=$run_id
run_url=$run_url
conclusion=$run_conclusion
output_directory=$out
EOF
