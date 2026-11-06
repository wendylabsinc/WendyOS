#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
BASE="${1:-origin/main}"
git rev-parse --verify "$BASE^{commit}" >/dev/null
if [ "$(buf --version)" != "1.75.0" ]; then
    echo "error: install buf 1.75.0 (same version as shared generation)" >&2
    exit 1
fi
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT
mkdir -p "$TEMP_DIR/base" "$TEMP_DIR/current"
git archive "$BASE" Proto | tar -x -C "$TEMP_DIR/base"
cp -R "$ROOT/Proto" "$TEMP_DIR/current/Proto"

# Build each side with its own shared pin. Local Agent definitions (including
# the historical v1 namespaces) are never replaced by imported shared types.
# Compare FILE rules directly; no baseline differences are globally exempted.
CONFIG='{"version":"v2","modules":[{"path":"."}],"breaking":{"use":["FILE"]}}'
build_image() (
    local side="$1" proto
    # shellcheck source=/dev/null
    source "$TEMP_DIR/$side/Proto/shared-grpc-api-contracts.sh"
    local args=()
    for proto in "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}"; do
        args+=(--path "$proto")
    done
    buf export \
        "https://github.com/wendylabsinc/service-protos.git#ref=$SHARED_GRPC_API_CONTRACTS_REF" \
        --exclude-imports "${args[@]}" --output "$TEMP_DIR/$side/shared"
    cp -R "$TEMP_DIR/$side/shared/." "$TEMP_DIR/$side/Proto/"
    cd "$TEMP_DIR/$side/Proto"
    buf build --config "$CONFIG" --path wendy/agent -o "$TEMP_DIR/$side.binpb"
)
build_image base
build_image current
buf breaking "$TEMP_DIR/current.binpb" --against "$TEMP_DIR/base.binpb" --config "$CONFIG"
