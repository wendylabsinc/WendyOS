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
AGENT_PKG="$MODULE/go/proto/gen/agentpb"

AGENT_PROTOS=(
    "wendy/agent/services/v1/shared.proto"
    "wendy/agent/services/v1/wendy_agent_v1_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_container_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_shell_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_provisioning_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_telemetry_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_bluetooth.proto"
    "wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_video_service.proto"
)

# Build M options for agent protos
AGENT_M_OPTS=""
for p in "${AGENT_PROTOS[@]}"; do
    AGENT_M_OPTS="$AGENT_M_OPTS --go_opt=M${p}=${AGENT_PKG}"
    AGENT_M_OPTS="$AGENT_M_OPTS --go-grpc_opt=M${p}=${AGENT_PKG}"
done

# ---- Wendy Agent v2 protos ----
V2_AGENT_PKG="$MODULE/go/proto/gen/agentpb/v2"

V2_AGENT_PROTOS=(
    "wendy/agent/services/v2/shared.proto"
    "wendy/agent/services/v2/device_info_service.proto"
    "wendy/agent/services/v2/agent_update_service.proto"
    "wendy/agent/services/v2/os_update_service.proto"
    "wendy/agent/services/v2/driver_service.proto"
    "wendy/agent/services/v2/wifi_service.proto"
    "wendy/agent/services/v2/bluetooth_service.proto"
    "wendy/agent/services/v2/container_service.proto"
    "wendy/agent/services/v2/provisioning_service.proto"
    "wendy/agent/services/v2/audio_service.proto"
    "wendy/agent/services/v2/telemetry_service.proto"
    "wendy/agent/services/v2/file_sync_service.proto"
    "wendy/agent/services/v2/mesh_service.proto"
    "wendy/agent/services/v2/ros2_service.proto"
    "wendy/agent/services/v2/timesync_service.proto"
    "wendy/agent/services/v2/build_service.proto"
    "wendy/agent/services/v2/sensor_pairing_service.proto"
    "wendy/agent/services/v2/sensor_service.proto"
)

V2_AGENT_M_OPTS=""
for p in "${V2_AGENT_PROTOS[@]}"; do
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go_opt=M${p}=${V2_AGENT_PKG}"
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go-grpc_opt=M${p}=${V2_AGENT_PKG}"
done

# sensor_service.proto (v2) imports wendy/lite/sensorlink.proto; map that
# import to the existing sensorlinkpb package so the v2 service reuses the
# shared SensorManifest/SensorFrame types instead of duplicating them.
SENSORLINK_PKG="$MODULE/go/proto/gen/sensorlinkpb"
V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go_opt=Mwendy/lite/sensorlink.proto=${SENSORLINK_PKG}"
V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go-grpc_opt=Mwendy/lite/sensorlink.proto=${SENSORLINK_PKG}"

# ---- OpenTelemetry protos ----
#
# These are NOT generated here. The canonical OTLP protos are already published
# as Go packages under go.opentelemetry.io/proto/otlp, and the Go protobuf
# runtime panics if the same proto file path is registered twice. Generating a
# second copy into this module made it impossible to link any dependency that
# pulls in the upstream packages (e.g. moby/buildkit's client).
#
# Instead we only map each OTLP proto file to its upstream Go package, so any
# Wendy proto that embeds an OTLP message imports upstream code. The .proto
# files under Proto/opentelemetry/ are still needed as protoc inputs (they are
# imported by the Wendy protos), but no Go code is emitted for them.
OTEL_PKG_PREFIX="go.opentelemetry.io/proto/otlp"

# proto file path -> upstream Go import path
declare -a OTEL_PROTO_PKGS=(
    "opentelemetry/proto/common/v1/common.proto=$OTEL_PKG_PREFIX/common/v1"
    "opentelemetry/proto/resource/v1/resource.proto=$OTEL_PKG_PREFIX/resource/v1"
    "opentelemetry/proto/logs/v1/logs.proto=$OTEL_PKG_PREFIX/logs/v1"
    "opentelemetry/proto/metrics/v1/metrics.proto=$OTEL_PKG_PREFIX/metrics/v1"
    "opentelemetry/proto/trace/v1/trace.proto=$OTEL_PKG_PREFIX/trace/v1"
    "opentelemetry/proto/collector/logs/v1/logs_service.proto=$OTEL_PKG_PREFIX/collector/logs/v1"
    "opentelemetry/proto/collector/metrics/v1/metrics_service.proto=$OTEL_PKG_PREFIX/collector/metrics/v1"
    "opentelemetry/proto/collector/trace/v1/trace_service.proto=$OTEL_PKG_PREFIX/collector/trace/v1"
)

OTEL_M_OPTS=""
for entry in "${OTEL_PROTO_PKGS[@]}"; do
    OTEL_M_OPTS="$OTEL_M_OPTS --go_opt=M${entry}"
    OTEL_M_OPTS="$OTEL_M_OPTS --go-grpc_opt=M${entry}"
done

# ---- Cloud protos ----
CLOUD_PKG="$MODULE/go/proto/gen/cloudpb"
CLOUD_PROTOS=(
    "cloud/apps.proto"
    "cloud/assets.proto"
    "cloud/certificates.proto"
    "cloud/deployments.proto"
    "cloud/mesh.proto"
    "cloud/notifications.proto"
    "cloud/organizations.proto"
    "cloud/remote_logging.proto"
    "cloud/tunnel.proto"
    "cloud/users.proto"
)

CLOUD_M_OPTS=""
for p in "${CLOUD_PROTOS[@]}"; do
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go_opt=M${p}=${CLOUD_PKG}"
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go-grpc_opt=M${p}=${CLOUD_PKG}"
done

# ---- Wendy System API protos ----
SYSTEM_PKG="$MODULE/go/proto/gen/systempb"
SYSTEM_PROTOS=(
    "wendy/system/v1/notifications.proto"
)

SYSTEM_M_OPTS=""
for p in "${SYSTEM_PROTOS[@]}"; do
    SYSTEM_M_OPTS="$SYSTEM_M_OPTS --go_opt=M${p}=${SYSTEM_PKG}"
    SYSTEM_M_OPTS="$SYSTEM_M_OPTS --go-grpc_opt=M${p}=${SYSTEM_PKG}"
done

# All M opts combined for cross-package imports
ALL_M_OPTS="$AGENT_M_OPTS $V2_AGENT_M_OPTS $OTEL_M_OPTS $CLOUD_M_OPTS $SYSTEM_M_OPTS"

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

echo "Generating Wendy Agent v2 protos..."
mkdir -p "$GEN_DIR/agentpb/v2"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/agentpb/v2" \
    --go_opt=module="$V2_AGENT_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/agentpb/v2" \
    --go-grpc_opt=module="$V2_AGENT_PKG" \
    "${V2_AGENT_PROTOS[@]}"

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

echo "Generating Wendy System API protos..."
mkdir -p "$GEN_DIR/systempb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/systempb" \
    --go_opt=module="$SYSTEM_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/systempb" \
    --go-grpc_opt=module="$SYSTEM_PKG" \
    ${SYSTEM_PROTOS[@]}

echo "Generating Wendy Lite protos..."
LITE_PKG="$MODULE/go/proto/gen/litepb"
mkdir -p "$GEN_DIR/litepb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/litepb" \
    --go_opt=module="$LITE_PKG" \
    --go_opt=Mwendy/lite/wendy_com_msg.proto="$LITE_PKG" \
    --go_opt=Mwendy/lite/wendy_conf.proto="$LITE_PKG" \
    wendy/lite/wendy_com_msg.proto \
    wendy/lite/wendy_conf.proto

# The tunnel protos import each other by bare filename so they can be moved
# to another project as-is; proto_path points inside wendy/lite accordingly.
echo "Generating Wendy Lite tunnel protos..."
TUNNEL_PKG="$MODULE/go/proto/gen/tunnelpb"
mkdir -p "$GEN_DIR/tunnelpb"
protoc \
    --proto_path="$PROTO_DIR/wendy/lite" \
    --go_out="$GEN_DIR/tunnelpb" \
    --go_opt=module="$TUNNEL_PKG" \
    --go_opt=Mwendy_com_tunnel_msg.proto="$TUNNEL_PKG" \
    --go_opt=Mwendy_com_tunnel_service.proto="$TUNNEL_PKG" \
    --go-grpc_out="$GEN_DIR/tunnelpb" \
    --go-grpc_opt=module="$TUNNEL_PKG" \
    --go-grpc_opt=Mwendy_com_tunnel_msg.proto="$TUNNEL_PKG" \
    --go-grpc_opt=Mwendy_com_tunnel_service.proto="$TUNNEL_PKG" \
    wendy_com_tunnel_msg.proto \
    wendy_com_tunnel_service.proto

echo "Generating Wendy Lite sensorlink protos..."
SENSORLINK_PKG="$MODULE/go/proto/gen/sensorlinkpb"
mkdir -p "$GEN_DIR/sensorlinkpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/sensorlinkpb" \
    --go_opt=module="$SENSORLINK_PKG" \
    "$PROTO_DIR/wendy/lite/sensorlink.proto"

echo "Proto generation complete!"
