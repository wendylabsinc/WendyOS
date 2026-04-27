#!/bin/bash
# Run from anywhere inside the swift/ directory tree.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/../WendyAgentCore"

PROTO_DIR="../Proto"

echo "Generating Wendy Agent gRPC code..."
rm -rf Sources/WendyAgentGRPC/Proto
mkdir -p Sources/WendyAgentGRPC/Proto
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/WendyAgentGRPC/Proto \
    --import-path "$PROTO_DIR" \
    -- \
    "$PROTO_DIR/wendy/agent/services/v1/shared.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_service.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_container_service.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_audio_service.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_provisioning_service.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_telemetry_service.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto" \
    "$PROTO_DIR/wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto"

echo "Generating OpenTelemetry gRPC code..."
rm -rf Sources/OpenTelemetryGRPC/Proto
mkdir -p Sources/OpenTelemetryGRPC/Proto
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/OpenTelemetryGRPC/Proto \
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
rm -rf Sources/WendyCloudGRPC/Proto
mkdir -p Sources/WendyCloudGRPC/Proto
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/WendyCloudGRPC/Proto \
    --import-path "$PROTO_DIR" \
    -- \
    "$PROTO_DIR/cloud/apps.proto" \
    "$PROTO_DIR/cloud/assets.proto" \
    "$PROTO_DIR/cloud/certificates.proto" \
    "$PROTO_DIR/cloud/deployments.proto" \
    "$PROTO_DIR/cloud/notifications.proto" \
    "$PROTO_DIR/cloud/organizations.proto" \
    "$PROTO_DIR/cloud/remote_logging.proto" \
    "$PROTO_DIR/cloud/users.proto"

echo "Proto generation complete."
