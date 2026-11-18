#!/bin/bash
# Run as the runner account. Xcode downloads may ask for an Apple login and 2FA.
# CI uses --base; initial host provisioning uses: 26.3 26.5 --ios-runtime
set -euo pipefail
[[ $(uname -s) == Darwin ]] || { echo 'This script requires macOS.' >&2; exit 1; }
[[ $(id -u) != 0 ]] || { echo 'Run as the runner account, not root.' >&2; exit 1; }
versions=()
ios_runtime=false
for argument in "$@"; do
  case "$argument" in
    --base) ;;
    --ios-runtime) ios_runtime=true ;;
    *)
      [[ "$argument" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo "Invalid Xcode version: $argument" >&2; exit 2; }
      versions+=("$argument")
      ;;
  esac
done
if $ios_runtime && (( ${#versions[@]} == 0 )); then
  echo '--ios-runtime requires an Xcode version.' >&2
  exit 2
fi

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
# Homebrew 7 asks before installing formulae unless this is set.
export HOMEBREW_NO_ASK=1
if [[ -n "${GITHUB_ENV:-}" ]]; then
  echo 'HOMEBREW_NO_ASK=1' >> "$GITHUB_ENV"
fi
if ! command -v brew >/dev/null 2>&1; then
  # The official installer also installs Command Line Tools on a fresh Mac.
  # Pin the installer so changes to its bootstrap logic are reviewed here.
  installer=$(mktemp)
  trap 'rm -f "$installer"' EXIT
  curl --fail --show-error --silent --location \
    https://raw.githubusercontent.com/Homebrew/install/e5b3c5834e3932c8aaa42deef4794076c9b0cbf0/install.sh \
    -o "$installer"
  NONINTERACTIVE=1 /bin/bash "$installer"
  rm -f "$installer"
  trap - EXIT
fi
brew --version
missing=()
for formula in git python@3.12; do
  brew list --versions "$formula" >/dev/null 2>&1 || missing+=("$formula")
done
if (( ${#missing[@]} )); then brew install "${missing[@]}"; fi
brew_prefix=$(brew --prefix)
python_bin="$(brew --prefix python@3.12)/libexec/bin"
export PATH="$python_bin:$brew_prefix/bin:$brew_prefix/sbin:$PATH"
git --version
python3 -c 'import http.server, ssl, sys; assert sys.version_info[:2] == (3, 12); print(sys.executable, sys.version)'
if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "$brew_prefix/bin" "$brew_prefix/sbin" "$python_bin" >> "$GITHUB_PATH"
fi

if (( ${#versions[@]} )); then
  # Homebrew can fall back to compiling xcodes, which itself requires Xcode.
  # Bootstrap from the official signed release instead, even on a fresh Mac.
  xcodes_bin="$HOME/.local/bin/xcodes"
  if [[ ! -x "$xcodes_bin" ]] || [[ $("$xcodes_bin" version) != 2.2.0 ]]; then
    download=$(mktemp -d)
    trap 'rm -rf "$download"' EXIT
    curl --fail --show-error --silent --location \
      https://github.com/XcodesOrg/xcodes/releases/download/2.2.0/xcodes.zip \
      -o "$download/xcodes.zip"
    echo "3c3ce6c550f079300c578b741e20ea0405fbe1eea808b8002ef591d568cea564  $download/xcodes.zip" | shasum -a 256 -c -
    unzip -q "$download/xcodes.zip" -d "$download"
    mkdir -p "$(dirname "$xcodes_bin")"
    install -m 755 "$download/xcodes" "$xcodes_bin"
    rm -rf "$download"
    trap - EXIT
  fi
  for version in "${versions[@]}"; do
    # Only download absent versions; rerunning provisioning is safe.
    if ! bash "$(dirname "$0")/select-macos-ci-xcode.sh" "$version" >/dev/null 2>&1; then
      "$xcodes_bin" install "$version"
    fi
    bash "$(dirname "$0")/select-macos-ci-xcode.sh" "$version"
  done
  if $ios_runtime; then
    # Load the selected version without modifying the system-wide selection.
    environment=$(mktemp)
    trap 'rm -f "$environment"' EXIT
    GITHUB_ENV="$environment" bash "$(dirname "$0")/select-macos-ci-xcode.sh" "$version"
    DEVELOPER_DIR=$(sed -n 's/^DEVELOPER_DIR=//p' "$environment")
    export DEVELOPER_DIR
    xcodebuild -downloadPlatform iOS
    xcrun simctl list runtimes
  fi
fi
