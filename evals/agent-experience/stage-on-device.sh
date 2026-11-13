#!/bin/sh
# Stage what the on-device image copies in: a linux/arm64 wendy built from this checkout, the skills it
# is tested with, and the commit they came from. Run from the repo root:
#   sh evals/agent-experience/stage-on-device.sh
# The cgo build needs libusb (gousb), so it runs in an arm64 golang container. The Go module cache lives
# in a named Docker volume so a second run is quick. Nothing is pruned.
set -eu

MIN_FREE_GIB=6
STAGE=evals/agent-experience/.stage
CACHE_VOLUME=wendy-evals-gomod
BUILD_CACHE_VOLUME=wendy-evals-gobuild

[ -d go ] && [ -d plugins/wendy-agentic-coding/skills ] || { echo "run this from the repo root" >&2; exit 1; }

free_gib=$(df -k . | awk 'NR==2 {print int($4 / 1048576)}')
if [ "$free_gib" -lt "$MIN_FREE_GIB" ]; then
  echo "only ${free_gib} GiB free; need at least ${MIN_FREE_GIB} GiB. Free some space and retry." >&2
  exit 1
fi

mkdir -p "$STAGE/treatment/plugins/wendy-agentic-coding"
rm -rf "$STAGE/treatment/plugins/wendy-agentic-coding/skills"
cp -R plugins/wendy-agentic-coding/skills "$STAGE/treatment/plugins/wendy-agentic-coding/skills"
git rev-parse HEAD > "$STAGE/treatment/REVISION"

docker volume create "$CACHE_VOLUME" >/dev/null
docker volume create "$BUILD_CACHE_VOLUME" >/dev/null
docker run --rm --platform linux/arm64 \
  -e GOTOOLCHAIN=auto -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=arm64 \
  -e GOMODCACHE=/gomod -e GOCACHE=/gobuild \
  -v "$CACHE_VOLUME":/gomod -v "$BUILD_CACHE_VOLUME":/gobuild \
  -v "$PWD":/src:ro -v "$PWD/$STAGE":/out -w /src/go \
  golang:1.27 sh -c '
    set -eu
    apt-get update -qq && apt-get install -y -qq --no-install-recommends libusb-1.0-0-dev pkg-config >/dev/null
    go build -o /out/wendy-linux-arm64 ./cmd/wendy
  '
echo "staged $STAGE (treatment revision $(cat "$STAGE/treatment/REVISION"))"
