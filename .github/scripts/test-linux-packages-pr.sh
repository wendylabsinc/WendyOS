#!/usr/bin/env bash
# Required PR execution in the existing Linux install safety job, no new runner.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-linux-packages-pr.sh NEW_EVIDENCE_DIRECTORY' >&2; exit 2; }
[[ $(uname -s) == Linux && ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || {
  echo 'package guest gate requires a GitHub-hosted Linux runner' >&2; exit 1;
}
[[ ! -e "$1" && ! -L "$1" ]] || { echo 'evidence output must be new' >&2; exit 2; }
mkdir -p "$1"
pr_package_output=$(realpath "$1")
pr_package_repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
pr_package_work=$(mktemp -d "$RUNNER_TEMP/wendy-pr-packages.XXXXXXXX")
trap 'rm -rf -- "$pr_package_work"' EXIT
pr_package_arch=$(go env GOHOSTARCH)
pr_package_version=2099.01.04-regression-package
bash "$pr_package_repo/.github/scripts/build-linux-release-cli.sh" "$pr_package_version" "$pr_package_arch" \
  "$pr_package_work/wendy" 2>&1 | tee "$pr_package_output/build-cli.log"
(
  cd "$pr_package_repo/go"
  CGO_ENABLED=0 GOOS=linux GOARCH="$pr_package_arch" go build \
    -ldflags "-s -w -X github.com/wendylabsinc/wendy/go/internal/shared/version.Version=$pr_package_version" \
    -o "$pr_package_work/wendy-agent" ./cmd/wendy-agent
) 2>&1 | tee "$pr_package_output/build-agent.log"
bash "$pr_package_repo/.github/scripts/build-linux-packages.sh" "$pr_package_version" "$pr_package_arch" \
  "$pr_package_work/wendy" "$pr_package_work/wendy-agent" "$pr_package_work/packages" \
  2>&1 | tee "$pr_package_output/package.log"
sudo env GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" RUNNER_TEMP="$RUNNER_TEMP" \
  python3 "$pr_package_repo/.github/scripts/run-installed-linux-package-guests.py" \
  --packages "$pr_package_work/packages" --expected-version "$pr_package_version" \
  --output "$pr_package_output/guests" 2>&1 | tee "$pr_package_output/guests.log"
