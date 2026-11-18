#!/bin/bash
# Run as the runner account. Leave the active Xcode installation unchanged.
set -euo pipefail
[[ $(uname -s) == Darwin ]] || { echo 'This script requires macOS.' >&2; exit 1; }
[[ $(id -u) != 0 ]] || { echo 'Run as the runner account, not root.' >&2; exit 1; }
for argument in "$@"; do
  [[ "$argument" == --base ]] || { echo "Unexpected argument: $argument. This script only provisions base tools." >&2; exit 2; }
done

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
