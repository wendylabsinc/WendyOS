#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROTO_DIR="$(cd "$SCRIPT_DIR/../../Proto" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/../../swift" && pwd)"

PROTOC_GEN_SWIFT="$(which protoc-gen-swift)"
PROTOC_GEN_GRPC_SWIFT="$(which protoc-gen-grpc-swift-2)"

# Output roots (matching existing directory layout)
AGENT_GRPC_OUT="$SWIFT_DIR/Sources/WendyAgentGRPC/Proto"
OTEL_GRPC_OUT="$SWIFT_DIR/Sources/OpenTelemetryGRPC/Proto"
CLOUD_GRPC_OUT="$SWIFT_DIR/Sources/WendyCloudGRPC/Proto"

generate() {
  local proto="$1"
  local out="$2"
  protoc \
    --plugin=protoc-gen-swift="$PROTOC_GEN_SWIFT" \
    --plugin=protoc-gen-grpc-swift="$PROTOC_GEN_GRPC_SWIFT" \
    --proto_path="$PROTO_DIR" \
    --swift_out="$out" \
    --swift_opt=Visibility=Public \
    --grpc-swift_out="$out" \
    --grpc-swift_opt=Visibility=Public \
    "$proto"
}

echo "Generating Wendy Agent Swift protos..."
AGENT_PROTOS=(
  "wendy/agent/services/v1/shared.proto"
  "wendy/agent/services/v1/wendy_agent_v1_service.proto"
  "wendy/agent/services/v1/wendy_agent_v1_container_service.proto"
  "wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
  "wendy/agent/services/v1/wendy_agent_v1_provisioning_service.proto"
  "wendy/agent/services/v1/wendy_agent_v1_telemetry_service.proto"
  "wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto"
  "wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto"
)
for proto in "${AGENT_PROTOS[@]}"; do
  generate "$proto" "$AGENT_GRPC_OUT"
done

echo "Generating OpenTelemetry Swift protos..."
OTEL_PROTOS=(
  "opentelemetry/proto/common/v1/common.proto"
  "opentelemetry/proto/resource/v1/resource.proto"
  "opentelemetry/proto/logs/v1/logs.proto"
  "opentelemetry/proto/metrics/v1/metrics.proto"
  "opentelemetry/proto/trace/v1/trace.proto"
  "opentelemetry/proto/collector/logs/v1/logs_service.proto"
  "opentelemetry/proto/collector/metrics/v1/metrics_service.proto"
  "opentelemetry/proto/collector/trace/v1/trace_service.proto"
)
for proto in "${OTEL_PROTOS[@]}"; do
  generate "$proto" "$OTEL_GRPC_OUT"
done

echo "Generating Wendy Cloud Swift protos..."
CLOUD_PROTOS=(
  "cloud/apps.proto"
  "cloud/assets.proto"
  "cloud/certificates.proto"
  "cloud/deployments.proto"
  "cloud/notifications.proto"
  "cloud/organizations.proto"
  "cloud/remote_logging.proto"
  "cloud/users.proto"
)
for proto in "${CLOUD_PROTOS[@]}"; do
  generate "$proto" "$CLOUD_GRPC_OUT"
done

echo "Swift proto generation complete!"
