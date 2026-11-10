#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PROTO_DIR="$(cd "$GO_DIR/../Proto" && pwd)"
GEN_DIR="$GO_DIR/proto/gen"

case "${1:-}" in
    ""|--shared-grpc-api-contracts-only|--check-shared-grpc-api-contracts) ;;
    *) echo "usage: $0 [--shared-grpc-api-contracts-only|--check-shared-grpc-api-contracts]" >&2; exit 1 ;;
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

MODULE="github.com/wendylabsinc/wendy"

# Migrated gRPC APIs share one source pin with embedded Swift.
source "$PROTO_DIR/shared-grpc-api-contracts.sh"
SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS=(
    "wendycloud/v2/apns.proto"
    "wendycloud/v2/device_access.proto"
    "wendycloud/v2/roles.proto"
    "wendycloud/v2/mesh.proto"
    "wendycloud/v2/users.proto"
    "wendycloud/v2/tunnel.proto"
    "wendycloud/v2/apps.proto"
    "wendycloud/v2/assets.proto"
    "wendycloud/v2/certificates.proto"
    "wendycloud/v2/deployments.proto"
    "wendycloud/v2/signed_request.proto"
    "wendycloud/v2/relay_mcp.proto"
    "wendycloud/v2/postbox.proto"
    "wendycloud/v2/registry.proto"
    "wendycloud/v2/organizations.proto"
    "wendycloud/v2/notifications.proto"
    "wendycloud/v2/device_enrollment.proto"
    "wendycloud/v2/pki_admin.proto"
    "wendy/options.proto"
)
# PKIAdminService returns the PKI ledger's own response messages.
# Preserve the existing Go package and message-only generation.
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

SHARED_GRPC_API_CONTRACTS_PROTOS=(
    "${SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS[@]}"
    "${PKI_PROTOS[@]}"
    "wendyauth/v1/envelope.proto"
    "${SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"
)
SHARED_GRPC_API_CONTRACTS_PATH_ARGS=()
for proto in "${SHARED_GRPC_API_CONTRACTS_PROTOS[@]}"; do
    SHARED_GRPC_API_CONTRACTS_PATH_ARGS+=(--path "$proto")
done
SHARED_GRPC_API_CONTRACTS_SCRATCH="$(mktemp -d)"
trap 'finish_shared_grpc_api_contracts "$?" "$SHARED_GRPC_API_CONTRACTS_SCRATCH"' EXIT
# Catalogue inspection is separate from generation's selected import tree.
buf export "$SHARED_GRPC_API_CONTRACTS_INPUT" --output "$SHARED_GRPC_API_CONTRACTS_SCRATCH/catalogue"
check_shared_grpc_api_contracts_sources "$PROTO_DIR" "$SHARED_GRPC_API_CONTRACTS_SCRATCH/catalogue"
SHARED_GRPC_API_CONTRACTS_DIR="$SHARED_GRPC_API_CONTRACTS_SCRATCH/selected"
buf export \
    "$SHARED_GRPC_API_CONTRACTS_INPUT" \
    "${SHARED_GRPC_API_CONTRACTS_PATH_ARGS[@]}" --output "$SHARED_GRPC_API_CONTRACTS_DIR"
for proto in "${SHARED_GRPC_API_CONTRACTS_PROTOS[@]}"; do
    if [ ! -f "$SHARED_GRPC_API_CONTRACTS_DIR/$proto" ]; then
        echo "error: selected shared proto missing from export: $proto" >&2
        exit 1
    fi
done
mkdir -p "$SHARED_GRPC_API_CONTRACTS_DIR/cloud"
for proto in "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
    mv "$SHARED_GRPC_API_CONTRACTS_DIR/$proto" "$SHARED_GRPC_API_CONTRACTS_DIR/cloud/$proto"
done

# ---- Wendy Agent protos ----
AGENT_PKG="$MODULE/go/proto/gen/agentpb"

AGENT_PROTOS=("${SHARED_GRPC_API_CONTRACTS_AGENT_V1_PROTOS[@]}")

# Build M options for agent protos
AGENT_M_OPTS=""
for p in "${AGENT_PROTOS[@]}"; do
    AGENT_M_OPTS="$AGENT_M_OPTS --go_opt=M${p}=${AGENT_PKG};agentpb"
    AGENT_M_OPTS="$AGENT_M_OPTS --go-grpc_opt=M${p}=${AGENT_PKG};agentpb"
done

# ---- Wendy Agent v2 protos ----
V2_AGENT_PKG="$MODULE/go/proto/gen/agentpb/v2"

V2_AGENT_PROTOS=("${SHARED_GRPC_API_CONTRACTS_AGENT_V2_PROTOS[@]}")

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

# sensor_service.proto (v2) imports wendylite/v1/sensorlink.proto; map that
# import to the existing sensorlinkpb package so the v2 service reuses the
# shared SensorManifest/SensorData types instead of duplicating them.
SENSORLINK_PKG="$MODULE/go/proto/gen/sensorlinkpb"
V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go_opt=Mwendylite/v1/sensorlink.proto=${SENSORLINK_PKG}"
V2_AGENT_M_OPTS="$V2_AGENT_M_OPTS --go-grpc_opt=Mwendylite/v1/sensorlink.proto=${SENSORLINK_PKG}"

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
# Cloud v2 and its signed_request method option share the same pinned inputs.
CLOUD_V2_PKG="$MODULE/go/proto/gen/cloudpb/v2"
CLOUD_V2_PROTOS=("${SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS[@]}")

# The import path ends in "/v2", which would name the Go package "v2"; the
# ";cloudpbv2" suffix pins a usable name without editing the vendored protos.
CLOUD_V2_M_OPTS=""
for p in "${CLOUD_V2_PROTOS[@]}"; do
    CLOUD_V2_M_OPTS="$CLOUD_V2_M_OPTS --go_opt=M${p}=${CLOUD_V2_PKG};cloudpbv2"
    CLOUD_V2_M_OPTS="$CLOUD_V2_M_OPTS --go-grpc_opt=M${p}=${CLOUD_V2_PKG};cloudpbv2"
done

# ---- Cloud-authorized relay protos ----
RELAY_PKG="$MODULE/go/proto/gen/relaypb"
WCOMRELAY_PKG="$MODULE/go/proto/gen/wcomrelaypb"
RELAY_M_OPTS="--go_opt=Mwendyauth/v1/envelope.proto=${AUTH_PKG} --go-grpc_opt=Mwendyauth/v1/envelope.proto=${AUTH_PKG}"
for entry in "wendycloud/tunnel/v2/tunnel.proto=$RELAY_PKG" "wendycloud/tunnel/lite/v1/relay.proto=$WCOMRELAY_PKG"; do
    RELAY_M_OPTS="$RELAY_M_OPTS --go_opt=M$entry --go-grpc_opt=M$entry"
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

RECORDING_PKG="$MODULE/go/proto/gen/recordingpb"
RECORDING_M_OPTS="--go_opt=Mwendy/agent/apps/v1/recording.proto=$RECORDING_PKG --go-grpc_opt=Mwendy/agent/apps/v1/recording.proto=$RECORDING_PKG"
ALL_M_OPTS="$AGENT_M_OPTS $V2_AGENT_M_OPTS $OTEL_M_OPTS $CLOUD_M_OPTS $CLOUD_V2_M_OPTS $PKI_M_OPTS $SYSTEM_M_OPTS $RECORDING_M_OPTS"

# CI regenerates the migrated gRPC APIs temporarily and compares their outputs.
if [ "${1:-}" = --shared-grpc-api-contracts-only ] || [ "${1:-}" = --check-shared-grpc-api-contracts ]; then
    SHARED_GRPC_API_CONTRACTS_OUT="$SHARED_GRPC_API_CONTRACTS_DIR/generated"
    mkdir -p "$SHARED_GRPC_API_CONTRACTS_OUT"
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go-grpc_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go-grpc_opt=module="$MODULE/go/proto/gen" \
        $CLOUD_V2_M_OPTS $PKI_M_OPTS "${SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS[@]}"
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go-grpc_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go-grpc_opt=module="$MODULE/go/proto/gen" \
        $RELAY_M_OPTS "${SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS[@]}"
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" wendyauth/v1/envelope.proto
    # Cloud is the client transport. PKI supplies messages, not direct clients.
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        $PKI_GO_M_OPTS "${PKI_PROTOS[@]}"
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go-grpc_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go-grpc_opt=module="$MODULE/go/proto/gen" \
        --proto_path="$PROTO_DIR" \
        $ALL_M_OPTS "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}"
    # The local Lite envelope imports sensorlink; check that generated edge too.
    LITE_PKG="$MODULE/go/proto/gen/litepb"
    protoc --proto_path="$PROTO_DIR/wendy/lite" --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go_opt=Mwendy_com_msg.proto="$LITE_PKG" --go_opt=Mwendy_conf.proto="$LITE_PKG" \
        --go_opt=Mwendylite/v1/sensorlink.proto="$SENSORLINK_PKG" \
        wendy_com_msg.proto wendy_conf.proto
    CLOUD_V1_SHARED_INPUTS=()
    for proto in "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
        CLOUD_V1_SHARED_INPUTS+=("cloud/$proto")
    done
    protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
        --go_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go_opt=module="$MODULE/go/proto/gen" \
        --go-grpc_out="$SHARED_GRPC_API_CONTRACTS_OUT" --go-grpc_opt=module="$MODULE/go/proto/gen" \
        $CLOUD_M_OPTS "${CLOUD_V1_SHARED_INPUTS[@]}"
    if [ "$1" = --check-shared-grpc-api-contracts ]; then
        for file in wendy_com_msg.pb.go wendy_conf.pb.go; do
            diff -u "$GEN_DIR/litepb/$file" "$SHARED_GRPC_API_CONTRACTS_OUT/litepb/$file"
        done
        for proto in "${SHARED_GRPC_API_CONTRACTS_PROTOS[@]}"; do
            case "$proto" in
                wendy/agent/services/v1/*) package="agentpb" ;;
                wendy/agent/services/v2/*) package="agentpb/v2" ;;
                wendy/agent/apps/*) package="recordingpb" ;;
                wendy/system/*) package="systempb" ;;
                wendylite/*) package="sensorlinkpb" ;;
                pkicore/*) package="pkicorepb" ;;
                wendyauth/*) package="wendyauthpb" ;;
                wendycloud/tunnel/v2/*) package="relaypb" ;;
                wendycloud/tunnel/lite/v1/*) package="wcomrelaypb" ;;
                */*) package="cloudpb/v2" ;;
                *) package="cloudpb" ;;
            esac
            name="${proto##*/}"
            file="${name%.proto}.pb.go"
            diff -u "$GEN_DIR/$package/$file" "$SHARED_GRPC_API_CONTRACTS_OUT/$package/$file"
            # Protos without services have no gRPC output. Compare either side
            # when present so missing or stale committed outputs still fail.
            file="${name%.proto}_grpc.pb.go"
            if [ -e "$GEN_DIR/$package/$file" ] || [ -e "$SHARED_GRPC_API_CONTRACTS_OUT/$package/$file" ]; then
                diff -u "$GEN_DIR/$package/$file" "$SHARED_GRPC_API_CONTRACTS_OUT/$package/$file"
            fi
        done
    else
        cp -R "$SHARED_GRPC_API_CONTRACTS_OUT/." "$GEN_DIR/"
    fi
    exit 0
fi

# Generate completely before replacing committed output, including on failure.
COMMITTED_GEN_DIR="$GEN_DIR"
GEN_DIR="$SHARED_GRPC_API_CONTRACTS_DIR/full-generated"

echo "Generating Wendy Agent protos..."
mkdir -p "$GEN_DIR/agentpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
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
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
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
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
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
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
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
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR/pkicorepb" \
    --go_opt=module="$PKI_PKG" \
    $PKI_GO_M_OPTS \
    "${PKI_PROTOS[@]}"

echo "Generating Wendy System API protos..."
mkdir -p "$GEN_DIR/systempb"
protoc \
    --proto_path="$PROTO_DIR" \
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR/systempb" \
    --go_opt=module="$SYSTEM_PKG" \
    $ALL_M_OPTS \
    --go-grpc_out="$GEN_DIR/systempb" \
    --go-grpc_opt=module="$SYSTEM_PKG" \
    ${SYSTEM_PROTOS[@]}

# The local Lite envelope retains its existing fields and imports canonical sensorlink.
echo "Generating Wendy Lite protos..."
LITE_PKG="$MODULE/go/proto/gen/litepb"
mkdir -p "$GEN_DIR/litepb"
protoc \
    --proto_path="$PROTO_DIR/wendy/lite" \
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR/litepb" \
    --go_opt=module="$LITE_PKG" \
    --go_opt=Mwendy_com_msg.proto="$LITE_PKG" \
    --go_opt=Mwendy_conf.proto="$LITE_PKG" \
    --go_opt=Mwendylite/v1/sensorlink.proto="$SENSORLINK_PKG" \
    wendy_com_msg.proto \
    wendy_conf.proto

echo "Generating Wendy Lite sensorlink protos..."
SENSORLINK_PKG="$MODULE/go/proto/gen/sensorlinkpb"
mkdir -p "$GEN_DIR/sensorlinkpb"
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$GEN_DIR/sensorlinkpb" \
    --go_opt=module="$SENSORLINK_PKG" \
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_opt=Mwendylite/v1/sensorlink.proto="$SENSORLINK_PKG" \
    wendylite/v1/sensorlink.proto

# Preserve the existing relay packages and RPC names using the shared pin.
protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR" --go_opt=module="$MODULE/go/proto/gen" \
    --go-grpc_out="$GEN_DIR" --go-grpc_opt=module="$MODULE/go/proto/gen" \
    $RELAY_M_OPTS "${SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS[@]}"
# Preserve the existing auth package from the identical shared envelope.
protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR" --go_opt=module="$MODULE/go/proto/gen" \
    --go_opt=Mwendyauth/v1/envelope.proto="$AUTH_PKG" wendyauth/v1/envelope.proto

# App recording envelopes (no generated SDK is needed for lightweight streams).
protoc --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" --go_out="$SHARED_GRPC_API_CONTRACTS_DIR" --go_opt=module="$MODULE/go" \
    --go_opt=Mwendy/agent/apps/v1/recording.proto="$RECORDING_PKG" wendy/agent/apps/v1/recording.proto

# Cloud-owned invite contract from cloud d840731bfb4a59bd6585efb359d67745d295a28d.
# UUID identifiers and SignedRequest differ from the deferred shared v1 API.
mkdir -p "$GEN_DIR/cloudinvitepb"
protoc --proto_path="$PROTO_DIR" \
    --proto_path="$SHARED_GRPC_API_CONTRACTS_DIR" \
    --go_out="$GEN_DIR/cloudinvitepb" --go_opt=module="$MODULE/go/proto/gen/cloudinvitepb" \
    --go-grpc_out="$GEN_DIR/cloudinvitepb" --go-grpc_opt=module="$MODULE/go/proto/gen/cloudinvitepb" \
    $ALL_M_OPTS \
    --go_opt=Morg_invites.proto="$MODULE/go/proto/gen/cloudinvitepb" \
    --go-grpc_opt=Morg_invites.proto="$MODULE/go/proto/gen/cloudinvitepb" org_invites.proto

cp -R "$SHARED_GRPC_API_CONTRACTS_DIR/proto/gen/." "$GEN_DIR/"
rm -rf "$COMMITTED_GEN_DIR"
mv "$GEN_DIR" "$COMMITTED_GEN_DIR"
echo "Proto generation complete!"
