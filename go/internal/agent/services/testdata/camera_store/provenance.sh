#!/bin/sh
# Shows that the golden layouts in this directory are what the camera adapter
# wrote before `store` existed.
#
# It runs the scenarios of data_camera_layout_golden_test.go on the tree of
# 757113c1e, the parent of the commit that added `store` (86f56a923), against
# the golden files as they are here. The row that names `store: video` is left
# out, because the field does not exist in that tree.
#
# The old tree is unpacked beside this checkout, at a fixed path so that a
# second run finds its packages in the Go build cache, and is removed on exit.
# It takes about 200 MB. If the commit is not in your clone, fetch the pull
# request that introduced `store`: git fetch origin pull/3089/head
set -eu

before_store=757113c1efbd7ccfbd4e6ac0114d90bd13d06a59
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
services=go/internal/agent/services
old_tree="$repo-golden-provenance"

if [ -e "$old_tree" ]; then
	echo "$old_tree already exists; remove it and run again" >&2
	exit 1
fi
mkdir "$old_tree"
trap 'rm -rf "$old_tree"' EXIT

git -C "$repo" archive "$before_store" go go.mod go.sum | tar -x -C "$old_tree"
sed '/"store video"/,/}},/d' "$repo/$services/data_camera_layout_golden_test.go" >"$old_tree/$services/data_camera_layout_golden_test.go"
mkdir -p "$old_tree/$services/testdata/camera_store"
cp "$here"/*_layout.json "$old_tree/$services/testdata/camera_store/"

cd "$old_tree/go"
go test ./internal/agent/services/ -count=1 -v -run 'TestDefaultCameraLayoutsMatchTheAdapterBeforeStore'
