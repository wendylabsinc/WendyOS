#!/usr/bin/env bash
# Usage: bash render.sh /path/to/wendy-g1-warehouse-turbopuffer-2026-09-29.mp4
set -euo pipefail
asset_dir="$(cd "$(dirname "$0")" && pwd)"
source_video="${1:?Pass the original G1 warehouse video path}"
render_tmp="$(mktemp -d)"
trap 'rm -rf "$render_tmp"' EXIT
rsvg-convert "$asset_dir/header.svg" -o "$render_tmp/header.png"
rsvg-convert "$asset_dir/simulation-label.svg" -o "$render_tmp/label.png"
ffmpeg -v error \
  -loop 1 -t 8 -i "$render_tmp/header.png" \
  -ss 16 -t 8 -i "$source_video" \
  -loop 1 -t 8 -i "$render_tmp/label.png" \
  -filter_complex '[1:v]crop=1080:924:0:360,scale=584:500[scene];[0:v][scene]overlay=616:0:shortest=1[base];[base][2:v]overlay=616:0:shortest=1,scale=960:400,fps=10,split[a][b];[a]palettegen=max_colors=160[p];[b][p]paletteuse=dither=none:diff_mode=rectangle[out]' \
  -map '[out]' -t 8 -loop 0 -y "$asset_dir/header.gif"
ffmpeg -v error -i "$asset_dir/header.gif" -frames:v 1 -y "$asset_dir/header.png"
