#!/bin/sh
# Shows that the golden layouts in this directory are what the camera adapter
# wrote before it stored a camera's own frames.
#
# It runs data_camera_layout_golden_test.go, unchanged, on the tree of
# 757113c1e, the parent of the commit that made the adapter store frames
# (86f56a923), against the golden files as they are here. It fails unless
# every scenario of that test ran there and passed, and every golden file here
# is read by one of them.
#
# The old tree is unpacked beside this checkout, at a fixed path so that a
# second run finds its packages in the Go build cache, and is removed on exit.
# It takes about 200 MB. If the commit is not in your clone, fetch the pull
# request that introduced it: git fetch origin pull/3089/head
set -eu

before_store=757113c1efbd7ccfbd4e6ac0114d90bd13d06a59
layout_test=TestDefaultCameraLayoutsMatchTheAdapterBeforeStore
here=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$here" rev-parse --show-toplevel)
services=go/internal/agent/services
driver="$repo/$services/data_camera_layout_golden_test.go"
old_tree="$repo-golden-provenance"

scenarios=$(grep -c '^		{"[a-z0-9_]*", "' "$driver" || true)
if [ "$scenarios" -eq 0 ]; then
	echo "found no layout scenarios in $driver" >&2
	exit 1
fi
for golden in "$here"/*_layout.json; do
	name=$(basename "$golden" _layout.json)
	if ! grep -q "^		{\"$name\", \"" "$driver"; then
		echo "no layout scenario reads $golden" >&2
		exit 1
	fi
done

if [ -e "$old_tree" ]; then
	echo "$old_tree already exists; remove it and run again" >&2
	exit 1
fi
mkdir "$old_tree"
log=$(mktemp)
trap 'rm -rf "$old_tree" "$log"' EXIT

git -C "$repo" archive "$before_store" go go.mod go.sum | tar -x -C "$old_tree"
cp "$driver" "$old_tree/$services/"
mkdir -p "$old_tree/$services/testdata/camera_store"
cp "$here"/*_layout.json "$old_tree/$services/testdata/camera_store/"

status=0
(cd "$old_tree/go" && go test ./internal/agent/services/ -count=1 -v -run "^$layout_test\$") >"$log" 2>&1 || status=$?
cat "$log"

passed=$(grep -c "^    --- PASS: $layout_test/" "$log" || true)
if [ "$status" -ne 0 ] || [ "$passed" -ne "$scenarios" ]; then
	echo "provenance not shown: go test exited $status and $passed of $scenarios layout scenarios passed on $before_store" >&2
	exit 1
fi
echo "all $scenarios layout scenarios ran on $before_store and match the golden files here"
