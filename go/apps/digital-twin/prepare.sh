#!/bin/sh
# Prepare a self-contained Wendy project without modifying the source checkout.
set -eu
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
output_dir=${1:?Usage: prepare.sh /absolute/output-directory}
case "$output_dir" in /*) ;; *) echo 'Use an absolute output directory' >&2; exit 1;; esac
if [ -e "$output_dir" ]; then echo 'Output must be a new directory' >&2; exit 1; fi
mkdir -p "$output_dir"
cp "$source_dir/go.mod" "$source_dir/go.sum" "$output_dir/"
rsync -a --exclude Build --exclude __pycache__ --exclude '*.xcframework' "$source_dir/go/" "$output_dir/go/"
cp "$source_dir/go/apps/digital-twin/Dockerfile" "$source_dir/go/apps/digital-twin/wendy.json" "$output_dir/"
printf 'Prepared %s\n' "$output_dir"
