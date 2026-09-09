#!/bin/sh
set -eu

exec max serve \
  --model /opt/models/all-MiniLM-L6-v2 \
  --served-model-name sentence-transformers/all-MiniLM-L6-v2 \
  --task embeddings_generation \
  --devices "$MAX_DEVICE" \
  --quantization-encoding float32 \
  --pool-embeddings \
  --max-batch-size 1 \
  --max-length 128 \
  --port 8000
