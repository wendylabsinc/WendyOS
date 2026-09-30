#!/bin/sh
# Pins the Wendy CLI release that the Wendy plugin's launcher installs.
#
#   scripts/pin-cli.sh [--launcher PATH] [--min TAG] [--local DIR] TAG
#
# Downloads the five wendy-cli release assets for TAG, checks each one's
# GitHub build provenance (gh attestation verify), and rewrites the pinned
# block of the launcher (default plugins/wendy/scripts/wendy) with TAG, the
# minimum CLI version it accepts on PATH (--min, default: keep the current
# one), and the assets' SHA-256 hashes. --local DIR hashes DIR/<asset> instead
# and skips the download and attestation (tests).
set -eu

REPO_SLUG="wendylabsinc/WendyOS"
BASE="https://github.com/$REPO_SLUG/releases/download"
PLATFORMS="darwin_arm64 linux_amd64 linux_arm64 windows_amd64 windows_arm64"
RELEASE_PATTERN='[0-9][0-9][0-9][0-9].[0-9][0-9].[0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]'

usage() {
  sed -n '3,4p' "$0" | sed 's/^# *//'
}

die() {
  echo "pin-cli: $1" >&2
  exit "${2:-1}"
}

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    openssl dgst -sha256 -r "$1" | awk '{ print $1 }'
  fi
}

is_release() {
  # shellcheck disable=SC2254 # RELEASE_PATTERN is a glob on purpose
  case "$1" in $RELEASE_PATTERN) return 0 ;; esac
  return 1
}

launcher="$(cd "$(dirname "$0")/.." && pwd)/plugins/wendy/scripts/wendy"
min=""
local_dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --launcher) launcher=$2; shift 2 ;;
    --min) min=$2; shift 2 ;;
    --local) local_dir=$2; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    -*) usage >&2; exit 2 ;;
    *) break ;;
  esac
done
[ $# -eq 1 ] || { usage >&2; exit 2; }
tag=$1

is_release "$tag" || die "$tag is not a release tag (YYYY.MM.DD-HHMMSS)" 2
[ -f "$launcher" ] || die "no launcher at $launcher" 2
if ! grep -q '^# >>> pinned by scripts/pin-cli.sh' "$launcher" || ! grep -q '^# <<< pinned$' "$launcher"; then
  die "$launcher has no pinned block" 2
fi
[ -n "$min" ] || min=$(sed -n 's/^MIN_VERSION="\(.*\)"$/\1/p' "$launcher")
[ -n "$min" ] || min=$tag
is_release "$min" || die "--min $min is not a release tag" 2
LC_ALL=C expr "$min" \<= "$tag" >/dev/null || die "--min $min is newer than $tag" 2

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
block="$work/block"
{
  echo '# >>> pinned by scripts/pin-cli.sh (do not edit by hand)'
  echo "CLI_VERSION=\"$tag\""
  echo "MIN_VERSION=\"$min\""
  echo "DOWNLOAD_BASE=\"$BASE\""
} >"$block"

for p in $PLATFORMS; do
  os=${p%_*}
  arch=${p#*_}
  ext=tar.gz
  [ "$os" != windows ] || ext=zip
  asset="wendy-cli-$os-$arch-$tag.$ext"
  if [ -n "$local_dir" ]; then
    file="$local_dir/$asset"
    [ -f "$file" ] || die "missing $file"
  else
    file="$work/$asset"
    echo "downloading $asset" >&2
    curl -fsSL -o "$file" "$BASE/$tag/$asset" || die "could not download $BASE/$tag/$asset"
    gh attestation verify "$file" --repo "$REPO_SLUG" >/dev/null ||
      die "$asset failed build-provenance verification; do not pin it"
  fi
  echo "SHA256_$p=\"$(sha256_of "$file")\"" >>"$block"
done
echo '# <<< pinned' >>"$block"

awk -v blockfile="$block" '
  /^# >>> pinned by scripts\/pin-cli\.sh/ { while ((getline line < blockfile) > 0) print line; skip = 1; next }
  /^# <<< pinned$/ { skip = 0; next }
  !skip { print }
' "$launcher" >"$work/launcher"
# Write in place so the file keeps its mode (0755).
cat "$work/launcher" >"$launcher"
echo "pinned $tag (min $min) in $launcher"
