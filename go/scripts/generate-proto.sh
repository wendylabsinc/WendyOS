#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROTO_DIR="$(cd "$GO_DIR/../Proto" && pwd)"
GEN_DIR="$GO_DIR/proto/gen"

export PATH="$PATH:$(go env GOPATH)/bin"

# Clean previous generated code
rm -rf "$GEN_DIR"

MODULE="github.com/wendylabsinc/wendy"

# ---- Wendy Agent protos ----
AGENT_PKG="$MODULE/proto/gen/agentpb"

AGENT_PROTOS=(
    "wendy/agent/services/v1/shared.proto"
    "wendy/agent/services/v1/wendy_agent_v1_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_container_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_provisioning_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_telemetry_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto"
)

# Build M options for agent protos
AGENT_M_OPTS=""
for p in "${AGENT_PROTOS[@]}"; do
    AGENT_M_OPTS="$AGENT_M_OPTS --go_opt=M${p}=${AGENT_PKG}"
    AGENT_M_OPTS="$AGENT_M_OPTS --go-grpc_opt=M${p}=${AGENT_PKG}"
done

# OTEL protos
OTEL_PKG="$MODULE/proto/gen/otelpb"
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

OTEL_M_OPTS=""
for p in "${OTEL_PROTOS[@]}"; do
    OTEL_M_OPTS="$OTEL_M_OPTS --go_opt=M${p}=${OTEL_PKG}"
    OTEL_M_OPTS="$OTEL_M_OPTS --go-grpc_opt=M${p}=${OTEL_PKG}"
done

# ---- Cloud protos ----
CLOUD_PKG="$MODULE/proto/gen/cloudpb"
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

CLOUD_M_OPTS=""
for p in "${CLOUD_PROTOS[@]}"; do
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go_opt=M${p}=${CLOUD_PKG}"
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go-grpc_opt=M${p}=${CLOUD_PKG}"
done

# All M opts combined for cross-package imports
ALL_M_OPTS="$AGENT_M_OPTS $OTEL_M_OPTS $CLOUD_M_OPTS"

echo "Generating OpenTelemetry protos..."
mkdir -p "$GEN_DIR/otelpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/otelpb" \
    --go_opt=module="$OTEL_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/otelpb" \
    --go-grpc_opt=module="$OTEL_PKG" \
    ${OTEL_PROTOS[@]}

echo "Generating Wendy Agent protos..."
mkdir -p "$GEN_DIR/agentpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/agentpb" \
    --go_opt=module="$AGENT_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/agentpb" \
    --go-grpc_opt=module="$AGENT_PKG" \
    ${AGENT_PROTOS[@]}

echo "Generating Wendy Cloud protos..."
mkdir -p "$GEN_DIR/cloudpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/cloudpb" \
    --go_opt=module="$CLOUD_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/cloudpb" \
    --go-grpc_opt=module="$CLOUD_PKG" \
    ${CLOUD_PROTOS[@]}

echo "Proto generation complete!"
