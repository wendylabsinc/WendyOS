#!/usr/bin/env bash
set -euo pipefail

# Restores a build directory archived by a trusted Swift Tests run on main.
#
# SECURITY: Build outputs are exchanged as run artifacts rather than through
# actions/cache. Any workflow that runs in main's context can write main-scoped
# cache entries, but an artifact can only be uploaded by the run that owns it.
# Only archives from runs of BUILD_CACHE_WORKFLOW_FILE on main whose event is
# listed in BUILD_CACHE_TRUSTED_EVENTS are accepted; those runs execute main's
# own workflow and never archive from pull requests. The residual trust anchor
# is GitHub's run metadata and main itself: a compromised main-context run of
# this workflow could publish a bad archive, but the same run could change main's
# source too. WDY-3798.
#
# A missing or unusable archive is never an error: the build then starts cold.

if [ "$#" -lt 2 ]; then
  echo "Usage: $(basename "$0") ARTIFACT_NAME PATH..." >&2
  exit 1
fi

: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${RUNNER_TEMP:?RUNNER_TEMP is required}"

SCRIPT_DIRECTORY="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export ARTIFACT_NAME="$1"
shift
RESTORE_PATHS=("$@")
: "${BUILD_CACHE_WORKFLOW_FILE:?BUILD_CACHE_WORKFLOW_FILE is required}"
export BUILD_CACHE_TRUSTED_EVENTS="${BUILD_CACHE_TRUSTED_EVENTS:-schedule workflow_dispatch}"
WORKFLOW_FILE="$BUILD_CACHE_WORKFLOW_FILE"

# Only events that run main's own workflow code can be trusted; pull-request
# style events never are, whatever a caller configures.
for event in $BUILD_CACHE_TRUSTED_EVENTS; do
  case "$event" in
    schedule|workflow_dispatch|push) ;;
    *)
      echo "::warning::Refusing to trust $event runs for build caches; building without the $ARTIFACT_NAME cache."
      exit 0
      ;;
  esac
done
DOWNLOAD_DIRECTORY="$RUNNER_TEMP/trusted-build-cache/$ARTIFACT_NAME"

# GNU tar keeps nanosecond mtimes; the runner's bsdtar truncates them to whole
# seconds, which makes Xcode and SwiftPM treat every restored file as changed.
if ! command -v zstd >/dev/null 2>&1 || ! command -v gtar >/dev/null 2>&1; then
  echo "::notice::zstd or GNU tar is unavailable; building without the $ARTIFACT_NAME cache."
  exit 0
fi

# Newest successful runs on main for each trusted event, newest first.
candidates=""
for event in $BUILD_CACHE_TRUSTED_EVENTS; do
  if ! runs="$(
    BUILD_CACHE_EVENT="$event" gh api \
      "repos/$GITHUB_REPOSITORY/actions/workflows/$WORKFLOW_FILE/runs?branch=main&event=$event&status=success&per_page=10" \
      --jq '
        .workflow_runs[]
        | select(
            .head_branch == "main"
            and .event == env.BUILD_CACHE_EVENT
            and .head_repository.full_name == env.GITHUB_REPOSITORY
          )
        | "\(.created_at) \(.id)"
      '
  )"; then
    echo "::notice::Could not list trusted $WORKFLOW_FILE runs; building without the $ARTIFACT_NAME cache."
    exit 0
  fi
  candidates="$candidates$runs"$'\n'
done
run_ids="$(printf '%s' "$candidates" | sort -r | awk 'NF { print $2 }')"

for run_id in $run_ids; do
  artifact="$(
    gh api "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/artifacts" \
      --jq '.artifacts[] | select(.name == env.ARTIFACT_NAME and .expired == false) | "\(.id) \(.digest // "")"' \
      | head -n 1
  )" || continue
  [ -n "$artifact" ] || continue
  read -r artifact_id expected_digest <<< "$artifact"

  # SECURITY: GitHub records the uploaded archive's SHA-256 server-side; verify
  # it before extracting anything. The digest proves which run produced the
  # archive, not that its entries are safe, so also reject entries that could
  # escape the workspace (before and after extracting), extract without
  # preserving owners or modes into a staging directory, and move only the
  # requested paths into the workspace.
  if [[ ! "$expected_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "::warning::$ARTIFACT_NAME from run $run_id has no SHA-256 digest; building without it."
    exit 0
  fi

  rm -rf "$DOWNLOAD_DIRECTORY"
  mkdir -p "$DOWNLOAD_DIRECTORY/stage"
  zip_path="$DOWNLOAD_DIRECTORY/artifact.zip"
  if ! gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact_id/zip" > "$zip_path"; then
    echo "::warning::Could not download $ARTIFACT_NAME from run $run_id; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi

  actual_digest="sha256:$(shasum -a 256 "$zip_path" | awk '{ print $1 }')"
  if [ "$actual_digest" != "$expected_digest" ]; then
    echo "::warning::$ARTIFACT_NAME from run $run_id does not match its recorded digest; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi

  archive="$DOWNLOAD_DIRECTORY/$ARTIFACT_NAME.tar.zst"
  if ! unzip -q "$zip_path" "$ARTIFACT_NAME.tar.zst" -d "$DOWNLOAD_DIRECTORY"; then
    echo "::warning::Could not unpack $ARTIFACT_NAME from run $run_id; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi
  if ! zstd -dc "$archive" | python3 "$SCRIPT_DIRECTORY/validate-build-cache-archive.py" --archive; then
    echo "::warning::$ARTIFACT_NAME from run $run_id contains unsafe entries; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi
  if ! zstd -dc "$archive" | gtar -xf - --no-same-owner --no-same-permissions -C "$DOWNLOAD_DIRECTORY/stage"; then
    echo "::warning::Could not extract $ARTIFACT_NAME from run $run_id; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi
  # Check what tar actually wrote, so parser differences cannot hide anything.
  if ! python3 "$SCRIPT_DIRECTORY/validate-build-cache-archive.py" --tree "$DOWNLOAD_DIRECTORY/stage"; then
    echo "::warning::$ARTIFACT_NAME from run $run_id extracted unsafe entries; building without it."
    rm -rf "$DOWNLOAD_DIRECTORY"
    exit 0
  fi

  for path in "${RESTORE_PATHS[@]}"; do
    if [ -e "$DOWNLOAD_DIRECTORY/stage/$path" ]; then
      rm -rf "$path"
      mkdir -p "$(dirname "$path")"
      mv "$DOWNLOAD_DIRECTORY/stage/$path" "$path"
    fi
  done
  rm -rf "$DOWNLOAD_DIRECTORY"
  echo "Restored $ARTIFACT_NAME ($expected_digest) from trusted run https://github.com/$GITHUB_REPOSITORY/actions/runs/$run_id"
  exit 0
done

echo "::notice::No trusted $ARTIFACT_NAME archive found; building without it."
