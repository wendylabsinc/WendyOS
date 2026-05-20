#!/usr/bin/env bash
set -euo pipefail

RUN_DIR=""
RESULT_PATH=""

usage() {
  cat <<EOF
Usage: $(basename "$0") (--run-dir DIR | --file FILE)

Sanitize Swift Testing xUnit XML by replacing XML 1.0-invalid control
characters with printable escape text. When changes are needed, the original
file is preserved next to the sanitized file with a .raw.xml suffix.

Options:
  --run-dir DIR  E2E run directory containing test-results.xml.
  --file FILE    xUnit XML file to sanitize.
  --help         Show this help message.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run-dir)
      RUN_DIR="$2"
      shift 2
      ;;
    --file)
      RESULT_PATH="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 64
      ;;
  esac
done

if [[ -n "$RUN_DIR" && -n "$RESULT_PATH" ]]; then
  echo "ERROR: pass either --run-dir or --file, not both." >&2
  usage >&2
  exit 64
fi

if [[ -n "$RUN_DIR" ]]; then
  RESULT_PATH="$RUN_DIR/test-results.xml"
fi

if [[ -z "$RESULT_PATH" ]]; then
  echo "ERROR: --run-dir or --file is required." >&2
  usage >&2
  exit 64
fi

if [[ ! -f "$RESULT_PATH" ]]; then
  exit 0
fi

raw_path="${RESULT_PATH%.xml}.raw.xml"
if [[ "$raw_path" == "$RESULT_PATH" ]]; then
  raw_path="$RESULT_PATH.raw"
fi

tmp_path="$(mktemp)"
trap 'rm -f "$tmp_path"' EXIT

perl -CSDA -0pe '
  s/([\x{00}-\x{08}\x{0B}\x{0C}\x{0E}-\x{1F}])/
    sprintf("\\u{%04X}", ord($1))
  /gex
' "$RESULT_PATH" > "$tmp_path"

if cmp -s "$RESULT_PATH" "$tmp_path"; then
  exit 0
fi

cp -p "$RESULT_PATH" "$raw_path"
cat "$tmp_path" > "$RESULT_PATH"
rm -f "$tmp_path"
trap - EXIT

echo "==> Sanitized Swift Testing xUnit results"
echo "    XML: $RESULT_PATH"
echo "    Raw: $raw_path"
