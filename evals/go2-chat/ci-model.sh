#!/usr/bin/env bash
# Executed in the official Ollama container with --network none and no devices.
set -euo pipefail
cd /eval
mkdir -p /reports
manifest=/models/manifests/registry.ollama.ai/library/qwen3-vl/4b-instruct
printf '%s  %s\n' "$WENDY_GO2_EVAL_MODEL_DIGEST" "$manifest" | sha256sum --check
sha256sum "$manifest" > /reports/model-sha256.txt
ollama --version > /reports/ollama-version.txt 2>&1
ollama serve > /reports/ollama.log 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true' EXIT
ready=false
for _ in {1..60}; do
  if ollama list > /reports/models.txt 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  cat /reports/ollama.log
  exit 1
fi
if [[ -z "${WENDY_GO2_EVAL_CASES:-}" ]]; then unset WENDY_GO2_EVAL_CASES; fi
export WENDY_GO2_EVAL_TIMEOUT_SECONDS=600
export WENDY_GO2_EVAL_CATALOG=/eval/catalog.json
export WENDY_GO2_EVAL_REPORT_DIR=/reports
export WENDY_GO2_EVAL_URL=http://127.0.0.1:11434/v1
export WENDY_GO2_EVAL_MODEL=qwen3-vl:4b-instruct
test -s "$WENDY_GO2_EVAL_CATALOG"
test -s testdata/go2-eval/cases.json
[[ "$(./chat.test -test.list='^TestGo2EvalModel$')" == TestGo2EvalModel ]]
./chat.test -test.run='^TestGo2EvalModel$' -test.v -test.timeout=50m 2>&1 | tee /reports/test.log
shopt -s nullglob
reports=(/reports/real-model-*.json)
[[ ${#reports[@]} -gt 0 ]]
