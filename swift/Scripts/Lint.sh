#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$SWIFT_DIR"

swift format lint --recursive --strict \
  WendyAgentCore/Sources \
  WendyAgentCore/Tests \
  WendyAgentMac/Sources
