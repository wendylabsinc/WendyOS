#!/usr/bin/env bash
# Consumer tool setup only; the shared runner owns check, generate and bump.
set -euo pipefail
case "$#:${1:-}" in
  1:check|1:generate|2:bump) ;;
  *) echo 'usage: bash scripts/contracts.sh check | generate | bump <full-40-hex-sha>' >&2; exit 1 ;;
esac
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
trap 'echo "error: contracts setup or runner failed; repair from the repository root: bash scripts/contracts.sh generate" >&2' ERR

# TEMPORARY: service-protos #198 (https://github.com/wendylabsinc/service-protos/pull/198)
# must land; switch this integration ref to main before merging this consumer.
readonly CONTRACTS_RUNNER_REF='35c8a2f4f1cfe01804043d66cd15545f6fc19a7d'
# Keep runner acquisition independent of the schema pin. The runner still fetches
# authoritative checker policy and the complete source catalogue from main.
# Preserve scoped Git authentication; clear only caller-repository redirection.
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_SHALLOW_FILE GIT_REPLACE_REF_BASE GIT_GRAFT_FILE
git init --bare --quiet "$scratch/source.git"
git -C "$scratch/source.git" fetch --quiet --depth=1 --no-tags \
  https://github.com/wendylabsinc/service-protos.git "$CONTRACTS_RUNNER_REF"
git -C "$scratch/source.git" archive FETCH_HEAD \
  tools/contracts.sh tools/contracts.py tools/contracts-requirements.txt | tar -x -C "$scratch"

python3 -m venv Build/Contracts/Python
Build/Contracts/Python/bin/python3 -m pip install --disable-pip-version-check \
  -r "$scratch/tools/contracts-requirements.txt"
go_path="$(go env GOPATH)"
export PATH="$PWD/Build/Contracts/Python/bin:$PATH:$go_path/bin"
# Keep the existing exact Swift generator dependencies and scratch directory.
for product in protoc-gen-swift protoc-gen-grpc-swift-2; do
  swift build --package-path swift/Scripts/ProtoGeneration \
    --scratch-path swift/Build/ProtoGeneration --jobs 4 --product "$product"
done
swift_bin="$(swift build --package-path swift/Scripts/ProtoGeneration \
  --scratch-path swift/Build/ProtoGeneration --show-bin-path)"
export PATH="$swift_bin:$PATH"
bash "$scratch/tools/contracts.sh" "$@"
