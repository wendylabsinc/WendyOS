#!/usr/bin/env bash
#
# Generate SPDX-JSON SBOMs for WendyOS release artifacts using Syft.
#
# Usage:
#   generate-sbom.sh binary <binary-path> <output-file>
#   generate-sbom.sh swift  <output-file> [--repo-root <dir>]
#   generate-sbom.sh source <output-file> [--repo-root <dir>]
#   generate-sbom.sh all    <binaries-dir> <out-dir> <version> [--repo-root <dir>]
#
# Env:
#   SYFT_BIN   syft executable (default: syft)
#   SWIFT_RESOLVED  Package.resolved from the release Xcode build
#   SWIFT_WORKSPACE_STATE  Xcode SourcePackages/workspace-state.json
#
set -euo pipefail

SYFT_BIN="${SYFT_BIN:-syft}"

usage() {
  sed -n '2,14p' "$0" >&2
  exit 2
}

# resolve --repo-root from trailing args; defaults to git toplevel or cwd.
repo_root() {
  local rr=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --repo-root)
        [[ $# -ge 2 ]] || { echo "error: --repo-root requires a value" >&2; exit 2; }
        rr="$2"; shift 2;;
      *) shift;;
    esac
  done
  if [[ -n "$rr" ]]; then printf '%s\n' "$rr"; return; fi
  git rev-parse --show-toplevel 2>/dev/null || pwd
}

scan_binary() {  # <binary-path> <output-file>
  local bin="$1" out="$2"
  [[ -f "$bin" ]] || { echo "error: binary not found: $bin" >&2; return 1; }
  mkdir -p "$(dirname "$out")"
  "$SYFT_BIN" scan "file:$bin" -o spdx-json > "$out"
}

scan_swift() {   # <output-file> <repo-root>
  local out="$1" rr="$2"
  local resolved="${SWIFT_RESOLVED:-$rr/swift/WendyAgent.xcworkspace/xcshareddata/swiftpm/Package.resolved}"
  python3 "$(dirname "${BASH_SOURCE[0]}")/swift-sbom.py" \
    "$resolved" "$rr/swift/WendyAgentCore/Package.swift" "$out" \
    --workspace-state "${SWIFT_WORKSPACE_STATE:-$rr/swift/Build/Xcode/SourcePackages/workspace-state.json}"
}

scan_source() {  # <output-file> <repo-root>
  local out="$1" rr="$2"
  mkdir -p "$(dirname "$out")"
  "$SYFT_BIN" scan "dir:$rr" \
    --exclude './.git/**' \
    --exclude '**/node_modules/**' \
    --exclude '**/.build/**' \
    --exclude './Examples/**' \
    -o spdx-json > "$out"
}

cmd="${1:-}"; [[ -n "$cmd" ]] || usage
shift || true

case "$cmd" in
  binary)
    [[ $# -ge 2 ]] || usage
    scan_binary "$1" "$2"
    ;;
  swift)
    [[ $# -ge 1 ]] || usage
    out="$1"; shift
    rr="$(repo_root "$@")"
    scan_swift "$out" "$rr"
    ;;
  source)
    [[ $# -ge 1 ]] || usage
    out="$1"; shift
    rr="$(repo_root "$@")"
    scan_source "$out" "$rr"
    ;;
  all)
    [[ $# -ge 3 ]] || usage
    bindir="$1"; outdir="$2"; version="$3"; shift 3
    rr="$(repo_root "$@")"
    mkdir -p "$outdir"
    shopt -s nullglob
    found=false
    for d in "$bindir"/*/; do
      artifact="$(basename "$d")"
      for b in "${d}wendy" "${d}wendy-agent" "${d}wendy.exe"; do
        [[ -f "$b" ]] || continue
        found=true
        scan_binary "$b" "$outdir/${artifact}-${version}.spdx.json"
      done
    done
    [[ "$found" == true ]] || { echo "error: no binaries under $bindir" >&2; exit 1; }
    scan_swift  "$outdir/wendy-swift-${version}.spdx.json"  "$rr"
    scan_source "$outdir/wendy-source-${version}.spdx.json" "$rr"
    ;;
  *)
    usage
    ;;
esac
