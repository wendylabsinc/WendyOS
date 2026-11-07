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
