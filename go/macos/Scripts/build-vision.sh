#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
APP_ROOT="${SRCROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
platform="${PLATFORM_NAME:-macosx}"
cache="$APP_ROOT/Build/VisualRecognition"
mkdir -p "$cache"
exec /usr/bin/perl -MFcntl=:flock -e 'open(my $lock, ">>", $ARGV[0]) or die $!; flock($lock, LOCK_EX) or die $!; shift @ARGV; system(@ARGV); exit($? >> 8)' "$cache/.lock" /bin/bash "$APP_ROOT/Scripts/build-vision-locked.sh" "$APP_ROOT" "$platform"
