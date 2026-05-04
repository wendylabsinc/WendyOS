#!/usr/bin/env bash
# DownloadVLM.sh
#
# Downloads an MLX vision-language model suitable for general computer-vision
# inference (evaluating a text prompt against a live camera frame) and places
# it in the HelloMLX/ directory next to this script's parent project.
#
# Model: mlx-community/gemma-3-27b-it-qat-4bit
#   • Gemma 3 27B is a large multimodal model with strong multi-image
#     reasoning, well suited for temporal scene summarisation.
#   • The 4-bit quantised variant weighs ~14 GB and fits comfortably in
#     the unified memory of a Mac with 32 GB RAM or more.
#
# Usage:
#   Scripts/DownloadVLM.sh
#
# Requirements:
#   pip install huggingface_hub   (provides the hf command)

set -euo pipefail

# ── Homebrew bootstrap ────────────────────────────────────────────────────────

eval "$(/opt/homebrew/bin/brew shellenv bash)"

# ── configuration ─────────────────────────────────────────────────────────────

HF_REPO="mlx-community/gemma-3-27b-it-qat-4bit"
MODEL_DIR="gemma-3-27b-it-qat-4bit"
SIZE_HINT="~14 GB"

# ── locate destination ────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DEST="$PROJECT_ROOT/Models/$MODEL_DIR"

# ── check dependencies ────────────────────────────────────────────────────────

if ! command -v hf &>/dev/null; then
    echo "❌  hf not found."
    echo ""
    echo "Install it with:"
    echo "    pip install huggingface_hub"
    echo ""
    echo "Or, if you use Homebrew Python:"
    echo "    pip3 install huggingface_hub"
    exit 1
fi

# ── download ──────────────────────────────────────────────────────────────────

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Model : $HF_REPO"
echo "  Size  : $SIZE_HINT (4-bit quantised MLX weights)"
echo "  Dest  : $DEST"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

mkdir -p "$DEST"

# Step 1 – populate the HF cache (no-op if already cached).
# This means a subsequent `git clean -fdx` won't require a re-download;
# the next run will copy from the cache instead of hitting the network.
hf download "$HF_REPO"

# Step 2 – copy from cache into the project's HelloMLX/ directory.
hf download \
    "$HF_REPO" \
    --local-dir "$DEST"

# Remove the .cache/ metadata folder created by hf; it's not part of the
# model and is not needed by MLX or the app.
rm -rf "$DEST/.cache"

echo ""
echo "✅  Model downloaded to:"
echo "    $DEST"
