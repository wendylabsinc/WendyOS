#!/usr/bin/env bash
# Compatibility for callers that source the old pin/check entrypoint.
_CONTRACTS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SHARED_GRPC_API_CONTRACTS_REF="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$_CONTRACTS_ROOT/contracts.json")" || return "$?"
check_shared_grpc_api_contracts_ancestry() {
  bash "$_CONTRACTS_ROOT/scripts/contracts.sh" check
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  exec bash "$_CONTRACTS_ROOT/scripts/contracts.sh" "$@"
fi
