#!/usr/bin/env bash
# Legacy entrypoint: both languages now share one complete generation/check.
set -euo pipefail
case "$#:${1:-}" in
  0:|1:--shared-grpc-api-contracts-only) mode=generate ;;
  1:--check-shared-grpc-api-contracts) mode=check ;;
  *) echo "usage: $0 [--shared-grpc-api-contracts-only|--check-shared-grpc-api-contracts]" >&2; exit 1 ;;
esac
exec bash "$(dirname "${BASH_SOURCE[0]}")/../../scripts/contracts.sh" "$mode"
