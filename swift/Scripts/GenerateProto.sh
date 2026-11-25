#!/bin/bash
# Run from anywhere inside the swift/ directory tree.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$SWIFT_DIR/WendyAgentCore"

case "${1:-}" in
  ""|--shared-grpc-api-contracts-only|--check-shared-grpc-api-contracts) ;;
  *) echo "usage: $0 [--shared-grpc-api-contracts-only|--check-shared-grpc-api-contracts]" >&2; exit 1 ;;
esac

LOCAL_PROTO_DIR="$(cd "$SWIFT_DIR/../Proto" && pwd)"
source "$LOCAL_PROTO_DIR/shared-grpc-api-contracts.sh"

PATH_ARGS=()
for proto in "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}" "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
  PATH_ARGS+=(--path "$proto")
done
if [ "$(buf --version)" != "1.75.0" ] || [ "$(protoc --version)" != "libprotoc 34.0" ]; then
  echo "error: install buf 1.75.0 and protoc 34.0 (same versions as Go generation)" >&2
  exit 1
fi
TEMP_DIR="$(mktemp -d)"
trap 'finish_shared_grpc_api_contracts "$?" "$TEMP_DIR"' EXIT
buf export "$SHARED_GRPC_API_CONTRACTS_INPUT" --output "$TEMP_DIR/catalogue"
check_shared_grpc_api_contracts_sources "$LOCAL_PROTO_DIR" "$TEMP_DIR/catalogue"
buf export \
  "$SHARED_GRPC_API_CONTRACTS_INPUT" \
  "${PATH_ARGS[@]}" --output "$TEMP_DIR/shared"
for proto in "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}" "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
  if [ ! -f "$TEMP_DIR/shared/$proto" ]; then
    echo "error: selected shared proto missing from export: $proto" >&2
    exit 1
  fi
done

# Keep existing proto names, local imports and module layout in temporary staging.
PROTO_DIR="$TEMP_DIR/Proto"
mkdir -p "$PROTO_DIR"
cp -R "$TEMP_DIR/shared/." "$PROTO_DIR/"
# Unmigrated imports (including OpenTelemetry) retain their local source behavior.
cp -R "$LOCAL_PROTO_DIR/." "$PROTO_DIR/"
for proto in "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
  mv "$PROTO_DIR/$proto" "$PROTO_DIR/cloud/$proto"
done
OUT_DIR="$TEMP_DIR/generated"
mkdir -p "$OUT_DIR/WendyAgentGRPC"
SHARED_SWIFT_PROTOS=(
  "$PROTO_DIR/wendy/agent/services/v1/shared.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_container_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_provisioning_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_telemetry_service.proto"
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto"
  "$PROTO_DIR/wendy/agent/services/v2/sensor_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/provisioning_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/robot_arm_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/bluetooth_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/local_mesh_admin_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/build_delivery_service.proto"
  "$PROTO_DIR/wendylite/v1/sensorlink.proto"
)
SHARED_SWIFT_MESSAGE_PROTOS=(
  "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_video_service.proto"
  "$PROTO_DIR/wendy/agent/services/v2/device_info_service.proto"
)

generate_grpc() {
  swift package --package-path "$SCRIPT_DIR/ProtoGeneration" \
    --scratch-path "$SWIFT_DIR/Build/ProtoGeneration" \
    --allow-writing-to-package-directory --allow-writing-to-directory "$TEMP_DIR" \
    generate-grpc-code-from-protos --protoc-path "$(command -v protoc)" "$@"
}

publicize_generated_imports() {
  find "$@" -name '*.swift' -print0 | xargs -0 perl -0pi -e '
    s/^(import (GRPCCore|GRPCProtobuf|SwiftProtobuf|Foundation|FoundationEssentials))$/public $1/mg;
    s/^public public import/public import/mg;
  '
}

echo "Generating Wendy Agent gRPC code..."
generate_grpc --access-level public --output-path "$OUT_DIR/WendyAgentGRPC" \
  --import-path "$PROTO_DIR" -- "${SHARED_SWIFT_PROTOS[@]}"
generate_grpc --no-clients --no-servers --access-level public \
  --output-path "$OUT_DIR/WendyAgentGRPC" --import-path "$PROTO_DIR" \
  -- "${SHARED_SWIFT_MESSAGE_PROTOS[@]}"

if [ "${1:-}" = --shared-grpc-api-contracts-only ]; then
  CLOUD_V1_SHARED_INPUTS=()
  for proto in "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
    CLOUD_V1_SHARED_INPUTS+=("$PROTO_DIR/cloud/$proto")
  done
  mkdir -p "$OUT_DIR/WendyCloudGRPC"
  generate_grpc --access-level public --output-path "$OUT_DIR/WendyCloudGRPC" \
    --import-path "$PROTO_DIR" -- "${CLOUD_V1_SHARED_INPUTS[@]}"
  publicize_generated_imports "$OUT_DIR/WendyAgentGRPC" "$OUT_DIR/WendyCloudGRPC"
  for input in "${SHARED_SWIFT_PROTOS[@]}" "${SHARED_SWIFT_MESSAGE_PROTOS[@]}" "${CLOUD_V1_SHARED_INPUTS[@]}"; do
    proto="${input#"$PROTO_DIR/"}"
    case "$proto" in
      cloud/*) module="WendyCloudGRPC" ;;
      *) module="WendyAgentGRPC" ;;
    esac
    suffixes=(pb.swift grpc.swift)
    for message in "${SHARED_SWIFT_MESSAGE_PROTOS[@]}"; do
      if [ "$input" = "$message" ]; then
        suffixes=(pb.swift)
      fi
    done
    for suffix in "${suffixes[@]}"; do
      file="${proto%.proto}.$suffix"
      committed="Sources/$module/Proto/$file"
      generated="$OUT_DIR/$module/$file"
      mkdir -p "$(dirname "$committed")"
      cp "$generated" "$committed"
    done
  done
  exit 0
fi


echo "Generating OpenTelemetry gRPC code..."
mkdir -p "$OUT_DIR/OpenTelemetryGRPC"
generate_grpc \
    --access-level public \
    --output-path "$OUT_DIR/OpenTelemetryGRPC" \
    --import-path "$PROTO_DIR" \
    -- \
    "$PROTO_DIR/opentelemetry/proto/common/v1/common.proto" \
    "$PROTO_DIR/opentelemetry/proto/resource/v1/resource.proto" \
    "$PROTO_DIR/opentelemetry/proto/logs/v1/logs.proto" \
    "$PROTO_DIR/opentelemetry/proto/metrics/v1/metrics.proto" \
    "$PROTO_DIR/opentelemetry/proto/trace/v1/trace.proto" \
    "$PROTO_DIR/opentelemetry/proto/collector/logs/v1/logs_service.proto" \
    "$PROTO_DIR/opentelemetry/proto/collector/metrics/v1/metrics_service.proto" \
    "$PROTO_DIR/opentelemetry/proto/collector/trace/v1/trace_service.proto"

echo "Generating Wendy Cloud gRPC code..."
mkdir -p "$OUT_DIR/WendyCloudGRPC"
generate_grpc \
    --access-level public \
    --output-path "$OUT_DIR/WendyCloudGRPC" \
    --import-path "$PROTO_DIR" \
    -- \
    "$PROTO_DIR/cloud/apps.proto" \
    "$PROTO_DIR/cloud/assets.proto" \
    "$PROTO_DIR/cloud/certificates.proto" \
    "$PROTO_DIR/cloud/deployments.proto" \
    "$PROTO_DIR/cloud/notifications.proto" \
    "$PROTO_DIR/cloud/organizations.proto" \
    "$PROTO_DIR/cloud/tunnel.proto" \
    "$PROTO_DIR/cloud/users.proto"

echo "Marking generated public API imports..."
publicize_generated_imports \
    "$OUT_DIR/WendyAgentGRPC" \
    "$OUT_DIR/OpenTelemetryGRPC" \
    "$OUT_DIR/WendyCloudGRPC"

if [ "${1:-}" = --check-shared-grpc-api-contracts ]; then
  drift=0
  for module in WendyAgentGRPC OpenTelemetryGRPC WendyCloudGRPC; do
    diff -ru "Sources/$module/Proto" "$OUT_DIR/$module" || drift=1
  done
  if [ "$drift" -ne 0 ]; then
    echo "error: generated Swift output differs; regenerate from repository root: bash swift/Scripts/GenerateProto.sh" >&2
    exit 1
  fi
  exit 0
fi

# Export, generation and postprocessing must all succeed before replacing outputs.
for module in WendyAgentGRPC OpenTelemetryGRPC WendyCloudGRPC; do
  rm -rf "Sources/$module/Proto"
  mkdir -p "Sources/$module"
  cp -R "$OUT_DIR/$module" "Sources/$module/Proto"
done

echo "Proto generation complete."
