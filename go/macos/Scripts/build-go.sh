#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
APP_ROOT="${SRCROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
REPO_ROOT="$(cd "$APP_ROOT/../.." && pwd)"
mkdir -p "$APP_ROOT/Build"
# Xcode can invoke both targets concurrently. Serialize archive replacement.
exec /usr/bin/perl -MFcntl=:flock -e 'open(my $lock, ">>", $ARGV[0]) or die $!; flock($lock, LOCK_EX) or die $!; shift @ARGV; system(@ARGV); exit($? >> 8)' "$APP_ROOT/Build/.lock" /bin/bash "$APP_ROOT/Scripts/build-go-locked.sh" "$APP_ROOT" "$REPO_ROOT"
