#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
ENTITLEMENTS_PATH="${ROOT_DIR}/Config/wendy-agent.entitlements"
IDENTIFIER="sh.wendy.agent.macos"
IDENTITY="${WENDY_CODESIGN_IDENTITY:--}"
BINARY_PATH="${1:-${ROOT_DIR}/.build/arm64-apple-macosx/release/wendy-agent}"

if [[ ! -f "${BINARY_PATH}" ]]; then
  echo "wendy-agent binary not found at: ${BINARY_PATH}" >&2
  echo "Pass the binary path explicitly or build it first with 'swift build -c release'." >&2
  exit 1
fi

if [[ ! -f "${ENTITLEMENTS_PATH}" ]]; then
  echo "entitlements file not found at: ${ENTITLEMENTS_PATH}" >&2
  exit 1
fi

args=(
  --force
  --sign "${IDENTITY}"
  --identifier "${IDENTIFIER}"
  --entitlements "${ENTITLEMENTS_PATH}"
)

if [[ "${IDENTITY}" != "-" ]]; then
  args+=(--timestamp --options runtime)
fi

codesign "${args[@]}" "${BINARY_PATH}"

echo "Signed ${BINARY_PATH}"
echo "  identifier: ${IDENTIFIER}"
echo "  identity:   ${IDENTITY}"
