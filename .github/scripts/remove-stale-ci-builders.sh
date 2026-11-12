#!/usr/bin/env bash
# Remove buildx builders that earlier CI jobs left on this runner.
#
# Integration and template jobs name their builders
# wendy-<run id>-<attempt>-<job>-<device>, and the CLI adds -oci and -mtls
# variants. A job that is cancelled or killed skips its cleanup trap, so the
# builder and its BuildKit state volume stay behind. 59 had accumulated on the
# self-hosted Mac by 2026-10-17 (WDY-3837).
#
# Only run this on a runner that executes one job at a time: any per-run
# builder present before the job creates its own then belongs to a finished job.
# SECURITY: a GitHub Actions runner process runs one job at a time, and the Mac
# hosts one runner (wendy-developer-macos-26-01). A second runner sharing its
# Docker daemon would break this; remove the call from macOS jobs before adding one.
# Best effort: never fail the job, but say when the listing fails and how many
# builders it found.
# DRY_RUN=true only lists what would be removed.
set -u

# Never guess which builders are active outside a GitHub run. The single-runner
# contract above is still required for builders belonging to other run IDs.
if [[ ! "${GITHUB_RUN_ID:-}" =~ ^[0-9]+$ ]]; then
  echo "::warning::Cannot identify the current run; skipped the stale-builder sweep."
  exit 0
fi

if ! names=$(docker buildx ls --no-trunc --format '{{.Name}}'); then
  echo "::warning::Could not list buildx builders; skipped the stale-builder sweep."
  exit 0
fi

found=0
while read -r name; do
  # A rerun or a later cleanup step must preserve every attempt of this run.
  [[ "$name" != "wendy-${GITHUB_RUN_ID}-"* ]] || continue
  # The listing also contains node names (<builder>0); skip anything that is
  # not a builder.
  docker buildx inspect "$name" >/dev/null 2>&1 || continue
  found=$((found + 1))
  if [[ "${DRY_RUN:-false}" == true ]]; then
    echo "Would remove stale builder $name"
    continue
  fi
  echo "Removing stale builder $name"
  docker buildx rm "$name" --force || true
done < <(grep -E '^wendy-[0-9]+-[0-9]+-[a-zA-Z0-9][a-zA-Z0-9_.-]*$' <<<"$names" | sort -u)
echo "Stale CI builders: $found"
exit 0
