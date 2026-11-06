# shellcheck shell=bash
# One immutable source pin for Go and embedded Swift generation.
SHARED_GRPC_API_CONTRACTS_REF="507da555d9a4851347734e916d30ee68c6c4bfa6"
# Canonical root files are staged under cloud/ to preserve existing descriptors.
SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS=(
    "apps.proto"
    "certificates.proto"
    "tunnel.proto"
    "users.proto"
)
SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS=(
    "wendy/agent/services/v1/wendy_agent_v1_audio_service.proto"
    "wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto"
    "wendy/agent/services/v2/file_sync_service.proto"
    "wendy/agent/services/v2/tunnel_service.proto"
)

SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS=(
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
    "wendycloud/v2/apns.proto"
    "wendycloud/v2/device_access.proto"
    "wendycloud/v2/roles.proto"
    "wendy/options.proto"
)
PKI_PROTOS=(
    "pkicore/v1/types.proto"
    "pkicore/v1/engine.proto"
    "pkicore/v1/fabric_management.proto"
    "pkicore/v1/fabric_relay.proto"
)
SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS=(
    "wendycloud/tunnel/v2/tunnel.proto"
    "wendyauth/v1/envelope.proto"
)
SHARED_GRPC_API_CONTRACTS_PROTOS=(
    "${SHARED_GRPC_API_CONTRACTS_CLOUD_PROTOS[@]}"
    "${PKI_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS[@]}"
)

check_shared_grpc_api_contracts_sources() {
    local proto_dir="$1" proto
    for proto in "${SHARED_GRPC_API_CONTRACTS_PROTOS[@]}"; do
        if [ -e "$proto_dir/$proto" ] || [ -L "$proto_dir/$proto" ]; then
            echo "error: remove Proto/$proto; it is generated from service-protos" >&2
            return 1
        fi
    done
    for proto in "${SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS[@]}"; do
        if [ -e "$proto_dir/cloud/$proto" ] || [ -L "$proto_dir/cloud/$proto" ]; then
            echo "error: remove Proto/cloud/$proto; it is generated from service-protos" >&2
            return 1
        fi
    done
}
