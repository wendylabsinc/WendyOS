#!/bin/sh
set -eu
cd "$(dirname "$0")"
uv sync --frozen
if [ "$#" -eq 0 ]; then
  set -- serve --local
fi
if [ "$1" = sim ] && [ "$(uname -s)" = Darwin ]; then
  exec .venv/bin/mjpython -m coke_demo "$@"
fi
exec .venv/bin/python -m coke_demo "$@"
