#!/usr/bin/env bash
set -euo pipefail

bundle_id="sh.wendy.WendyAgentMac"

services=(
  All
  Camera
  Microphone
  BluetoothAlways
)

echo "Resetting TCC entries for ${bundle_id}"

for service in "${services[@]}"; do
  echo "- Resetting ${service}"
  tccutil reset "$service" "$bundle_id" || true
done

echo "Done"
