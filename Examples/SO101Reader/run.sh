#!/bin/sh
set -eu
cd "$(dirname "$0")"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH:/usr/sbin:/sbin"
task_python="${SO101_PYTHON:-python3}"
"$task_python" -c 'import sys; sys.version_info >= (3, 10) or sys.exit("SO101Reader needs Python 3.10+; set SO101_PYTHON to an installed interpreter")'
exec "$task_python" -u reader.py "$@"
