#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROTO_DIR="$(cd "$GO_DIR/../Proto" && pwd)"
GEN_DIR="$GO_DIR/proto/gen"

case "${1:-}" in
    ""|--mesh-only|--check-mesh) ;;
    *) echo "usage: $0 [--mesh-only|--check-mesh]" >&2; exit 1 ;;
esac

export PATH="$PATH:$(go env GOPATH)/bin"

# ---- Pinned generator versions ----
#
# The generated tree is committed, and every generator stamps its own version into a
# comment in each file. So a mismatch does not fail — it rewrites files that nobody
# meant to touch, and the real change disappears into the noise. Before this check the
# tree had drifted across four protoc versions (v5.28.3, v7.34.0, v7.35.1, v7.36.0),
# each left behind by whoever last regenerated a subset.
#
# To upgrade deliberately: change these pins, regenerate everything, and commit the
# result on its own. WENDY_PROTO_SKIP_VERSION_CHECK=1 bypasses this for a one-off.
REQUIRED_BUF="1.75.0"
REQUIRED_PROTOC="34.0"
REQUIRED_PROTOC_GEN_GO="v1.36.11"
REQUIRED_PROTOC_GEN_GO_GRPC="1.6.2"

check_version() {
    local tool="$1" want="$2" got="$3" install="$4"
    if [ -z "$got" ]; then
        echo "error: $tool not found on PATH" >&2
        echo "  install: $install" >&2
        return 1
    fi
    if [ "$got" != "$want" ]; then
        echo "error: $tool is $got, this tree is generated with $want" >&2
        echo "  install: $install" >&2
        echo "  (or set WENDY_PROTO_SKIP_VERSION_CHECK=1 to proceed anyway)" >&2
        return 1
    fi
    printf '  %-20s %s\n' "$tool" "$got"
}

if [ "${WENDY_PROTO_SKIP_VERSION_CHECK:-0}" != "1" ]; then
    echo "Checking generator versions..."
    failed=0
    check_version buf "$REQUIRED_BUF" "$(buf --version 2>/dev/null)" \
        "https://github.com/bufbuild/buf/releases/tag/v$REQUIRED_BUF" || failed=1
    check_version protoc "$REQUIRED_PROTOC" \
        "$(protoc --version 2>/dev/null | awk '{print $2}')" \
        "https://github.com/protocolbuffers/protobuf/releases/tag/v$REQUIRED_PROTOC (no brew formula for this version)" || failed=1
    check_version protoc-gen-go "$REQUIRED_PROTOC_GEN_GO" \
        "$(protoc-gen-go --version 2>/dev/null | awk '{print $2}')" \
        "go install google.golang.org/protobuf/cmd/protoc-gen-go@$REQUIRED_PROTOC_GEN_GO" || failed=1
    check_version protoc-gen-go-grpc "$REQUIRED_PROTOC_GEN_GO_GRPC" \
        "$(protoc-gen-go-grpc --version 2>/dev/null | awk '{print $2}')" \
        "go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v$REQUIRED_PROTOC_GEN_GO_GRPC" || failed=1
    if [ "$failed" -ne 0 ]; then
        echo "" >&2
        echo "Refusing to generate: a mismatch rewrites every generated file with a new" >&2
        echo "version stamp, burying the change you actually made." >&2
        exit 1
    fi
fi

# This one shared API comes from service-protos, never a maintained local copy.
MESH_PROTO="wendycloud/v2/mesh.proto"
if [ -e "$PROTO_DIR/$MESH_PROTO" ] || [ -L "$PROTO_DIR/$MESH_PROTO" ]; then
    echo "error: remove Proto/$MESH_PROTO; it is generated from service-protos" >&2
    exit 1
fi
SHARED_PROTO_DIR="$(mktemp -d)"
trap 'rm -rf "$SHARED_PROTO_DIR"' EXIT
buf export \
    "https://github.com/wendylabsinc/service-protos.git#ref=3f29f59fc72f692a52b91a03a14d465d5e8dbfc7" \
    --path "$MESH_PROTO" --output "$SHARED_PROTO_DIR"

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
    "wendy/agent/services/v2/local_mesh_admin_service.proto"
    "wendy/agent/services/v2/ros2_service.proto"
    "wendy/agent/services/v2/timesync_service.proto"
    "wendy/agent/services/v2/build_service.proto"
    "wendy/agent/services/v2/calibrated_frame_service.proto"
    "wendy/agent/services/v2/robot_arm_service.proto"
    "wendy/agent/services/v2/sensor_pairing_service.proto"
    "wendy/agent/services/v2/unitree_service.proto"
    "wendy/agent/services/v2/unitree_robot_service.proto"
    "wendy/agent/services/v2/sensor_service.proto"
    "wendy/agent/services/v2/tunnel_service.proto"
    "wendy/agent/services/v2/data_service.proto"
)

V2_AGENT_M_OPTS=""
for p in "${V2_AGENT_PROTOS[@]}"; do
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go_opt=M${p}=${V2_AGENT_PKG}"
    V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go-grpc_opt=M${p}=${V2_AGENT_PKG}"
done

# calibrated_frame_service.proto (v2) imports the v1 video service proto for
# RawFormat: a colour plane's layout is exactly what a raw tap subscriber
# already receives, and two descriptions of the same bytes drift apart. The v1
# file is mapped by AGENT_M_OPTS (folded into ALL_M_OPTS below), so the
# generated v2 package imports agentpb rather than re-declaring the message.

# sensor_service.proto (v2) imports wendy/lite/sensorlink.proto; map that
# import to the existing sensorlinkpb package so the v2 service reuses the
# shared SensorManifest/SensorData types instead of duplicating them.
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
    "cloud/data_ingest.proto"
    "cloud/deployments.proto"
    "cloud/mesh.proto"
    "cloud/notifications.proto"
    "cloud/organizations.proto"
    "cloud/tunnel.proto"
    "cloud/users.proto"
)

CLOUD_M_OPTS=""
for p in "${CLOUD_PROTOS[@]}"; do
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go_opt=M${p}=${CLOUD_PKG}"
    CLOUD_M_OPTS="$CLOUD_M_OPTS --go-grpc_opt=M${p}=${CLOUD_PKG}"
done

# ---- Cloud v2 protos ----
# Except for mesh.proto (exported above), wendycloud.v2 is vendored beside v1.
# Paths match service-protos, including DeviceEnrollmentService, Cloud-owned
# though it is.
#
# Pin: the local wendycloud/v2/*.proto and wendy/options.proto are service-protos master
# cfa7f45d (merged #145, registry vectors), except postbox.proto,
# deployments.proto and registry.proto: master 65830a0a (merged #151,
# WDY-4015 operator CA and instruction validity). device_enrollment.proto is not in
# service-protos: it is cloud #757 head 5e3003e4
# (cloud-proto/device_enrollment.proto), including enrollment hardware.
# Re-pin: copy those files from
# the new heads with `git show <sha>:<path>`, run `make proto`, and update the
# SHAs here. wendy/options.proto (the signed_request method option) generates
# into cloudpbv2 too, so the option and SignedRequest live in one Go package.
CLOUD_V2_PKG="$MODULE/go/proto/gen/cloudpb/v2"
CLOUD_V2_PROTOS=(
    "wendy/options.proto"
    "wendycloud/v2/signed_request.proto"
    "wendycloud/v2/relay_mcp.proto"
    "wendycloud/v2/device_enrollment.proto"
    "wendycloud/v2/pki_admin.proto"
    "wendycloud/v2/registry.proto"
    "wendycloud/v2/apps.proto"
    "wendycloud/v2/assets.proto"
    "wendycloud/v2/certificates.proto"
    "wendycloud/v2/deployments.proto"
    "wendycloud/v2/mesh.proto"
    "wendycloud/v2/notifications.proto"
    "wendycloud/v2/organizations.proto"
    "wendycloud/v2/tunnel.proto"
    "wendycloud/v2/users.proto"
    "wendycloud/v2/postbox.proto"
)

# The import path ends in "/v2", which would name the Go package "v2"; the
# ";cloudpbv2" suffix pins a usable name without editing the vendored protos.
CLOUD_V2_M_OPTS=""
for p in "${CLOUD_V2_PROTOS[@]}"; do
    CLOUD_V2_M_OPTS="$CLOUD_V2_M_OPTS --go_opt=M${p}=${CLOUD_V2_PKG};cloudpbv2"
    CLOUD_V2_M_OPTS="$CLOUD_V2_M_OPTS --go-grpc_opt=M${p}=${CLOUD_V2_PKG};cloudpbv2"
done

# CI regenerates only mesh into a temporary directory and compares both outputs.
if [ "${1:-}" = --mesh-only ] || [ "${1:-}" = --check-mesh ]; then
    MESH_OUT="$GEN_DIR/cloudpb/v2"
    if [ "$1" = --check-mesh ]; then
        MESH_OUT="$SHARED_PROTO_DIR/generated"
    fi
    mkdir -p "$MESH_OUT"
    protoc --proto_path="$SHARED_PROTO_DIR" \
        --go_out="$MESH_OUT" --go_opt=module="$CLOUD_V2_PKG" \
        --go-grpc_out="$MESH_OUT" --go-grpc_opt=module="$CLOUD_V2_PKG" \
        $CLOUD_V2_M_OPTS "$MESH_PROTO"
    if [ "$1" = --check-mesh ]; then
        for file in mesh.pb.go mesh_grpc.pb.go; do
            diff -u "$GEN_DIR/cloudpb/v2/$file" "$MESH_OUT/$file"
        done
    fi
    exit 0
fi

# PKIAdminService returns the PKI ledger's own response messages. These four
# complete contracts are vendored from service-protos
# 6ad12f11bc3359ff902d586fd601a5530792febe, the Cloud API's protocol pin.
PKI_PKG="$MODULE/go/proto/gen/pkicorepb"
AUTH_PKG="$MODULE/go/proto/gen/wendyauthpb"
PKI_PROTOS=(
    "pkicore/v1/types.proto"
    "pkicore/v1/engine.proto"
    "pkicore/v1/fabric_management.proto"
    "pkicore/v1/fabric_relay.proto"
)
PKI_M_OPTS="--go_opt=Mwendyauth/v1/envelope.proto=${AUTH_PKG} --go-grpc_opt=Mwendyauth/v1/envelope.proto=${AUTH_PKG}"
PKI_GO_M_OPTS="--go_opt=Mwendyauth/v1/envelope.proto=${AUTH_PKG}"
for p in "${PKI_PROTOS[@]}"; do
    PKI_M_OPTS="$PKI_M_OPTS --go_opt=M${p}=${PKI_PKG};pkicorepb"
    PKI_M_OPTS="$PKI_M_OPTS --go-grpc_opt=M${p}=${PKI_PKG};pkicorepb"
    PKI_GO_M_OPTS="$PKI_GO_M_OPTS --go_opt=M${p}=${PKI_PKG};pkicorepb"
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
ALL_M_OPTS="$AGENT_M_OPTS $V2_AGENT_M_OPTS $OTEL_M_OPTS $CLOUD_M_OPTS $CLOUD_V2_M_OPTS $PKI_M_OPTS $SYSTEM_M_OPTS"

# Clean previous generated code only for a full regeneration.
rm -rf "$GEN_DIR"

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

echo "Generating Wendy Cloud v2 protos..."
mkdir -p "$GEN_DIR/cloudpb/v2"
protoc \
    --proto_path="$PROTO_DIR" \
    --proto_path="$SHARED_PROTO_DIR" \
    --go_out="$GEN_DIR/cloudpb/v2" \
    --go_opt=module="$CLOUD_V2_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/cloudpb/v2" \
    --go-grpc_opt=module="$CLOUD_V2_PKG" \
    ${CLOUD_V2_PROTOS[@]}

echo "Generating PKI administration protos..."
mkdir -p "$GEN_DIR/pkicorepb"
# Cloud is the only client transport; PKI files supply message descriptors,
# not direct engine or fabric clients.
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/pkicorepb" \
    --go_opt=module="$PKI_PKG" \
    $PKI_GO_M_OPTS \
    "${PKI_PROTOS[@]}"

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

# wendy_com_msg.proto imports sensorlink.proto by bare filename, for the same
# reason the relay protos do: in the wendy-lite project the two sit side by
# side in one directory, and the file is shared with that project verbatim. So
# proto_path points inside wendy/lite here too. sensorlink.proto is an input
# only -- its Go code comes from the sensorlinkpb generation further down,
# which the agent's v2 sensor service shares. The wendy-lite project generates
# it into a sensorlinkpb package of its own under the same import path, so the
# client code shared with that project names these types identically on both
# sides.
echo "Generating Wendy Lite protos..."
LITE_PKG="$MODULE/go/proto/gen/litepb"
mkdir -p "$GEN_DIR/litepb"
protoc \
    --proto_path="$PROTO_DIR/wendy/lite" \
    --go_out="$GEN_DIR/litepb" \
    --go_opt=module="$LITE_PKG" \
    --go_opt=Mwendy_com_msg.proto="$LITE_PKG" \
    --go_opt=Mwendy_conf.proto="$LITE_PKG" \
    --go_opt=Msensorlink.proto="$SENSORLINK_PKG" \
    wendy_com_msg.proto \
    wendy_conf.proto

echo "Generating Wendy Lite sensorlink protos..."
SENSORLINK_PKG="$MODULE/go/proto/gen/sensorlinkpb"
mkdir -p "$GEN_DIR/sensorlinkpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/sensorlinkpb" \
    --go_opt=module="$SENSORLINK_PKG" \
    --go_opt=Mwendy/lite/sensorlink.proto="$SENSORLINK_PKG" \
    "$PROTO_DIR/wendy/lite/sensorlink.proto"

echo "Proto generation complete!"

# Cloud-authorized relay contract (service-protos fe42be2). Crypto domain
# separators remain v1; the RPC package and UUID asset identifiers are v2.
RELAY_PKG="$MODULE/go/proto/gen/relaypb"
AUTH_PKG="$MODULE/go/proto/gen/wendyauthpb"
protoc --proto_path="$PROTO_DIR" \
    --go_out="$GO_DIR" --go_opt=module="$MODULE/go" \
    --go_opt=Mwendycloud/tunnel/v2/tunnel.proto="$RELAY_PKG" \
    --go_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" \
    --go-grpc_out="$GO_DIR" --go-grpc_opt=module="$MODULE/go" \
    --go-grpc_opt=Mwendycloud/tunnel/v2/tunnel.proto="$RELAY_PKG" \
    --go-grpc_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" \
    wendycloud/tunnel/v2/tunnel.proto wendyauth/v1/envelope.proto

# Packaged Lite relay contract from service-protos.
WCOMRELAY_PKG="$MODULE/go/proto/gen/wcomrelaypb"
protoc --proto_path="$PROTO_DIR" \
    --go_out="$GO_DIR" --go_opt=module="$MODULE/go" \
    --go_opt=Mwendycloud/tunnel/lite/v1/relay.proto="$WCOMRELAY_PKG" \
    --go_opt=Mwendycloud/tunnel/v2/tunnel.proto="$RELAY_PKG" \
    --go_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" \
    --go-grpc_out="$GO_DIR" --go-grpc_opt=module="$MODULE/go" \
    --go-grpc_opt=Mwendycloud/tunnel/lite/v1/relay.proto="$WCOMRELAY_PKG" \
    --go-grpc_opt=Mwendycloud/tunnel/v2/tunnel.proto="$RELAY_PKG" \
    --go-grpc_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" \
    wendycloud/tunnel/lite/v1/relay.proto

# App recording envelopes (no generated SDK is needed for lightweight streams).
protoc --proto_path="$PROTO_DIR" --go_out="$GO_DIR" --go_opt=module="$MODULE/go" \
    wendy/agent/apps/v1/recording.proto
