# shellcheck shell=bash
# One immutable source pin for Go and embedded Swift generation.
SHARED_GRPC_API_CONTRACTS_REF="81be2ae44f7ae2e83b1616eaa824b6e6372543aa"
# Canonical root files are staged under cloud/ to preserve existing descriptors.
SHARED_GRPC_API_CONTRACTS_CLOUD_V1_PROTOS=(
    "apps.proto"
    "certificates.proto"
    "tunnel.proto"
    "users.proto"
)
SHARED_GRPC_API_CONTRACTS_AGENT_V1_PROTOS=(
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
SHARED_GRPC_API_CONTRACTS_AGENT_V2_PROTOS=(
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
SHARED_GRPC_API_CONTRACTS_AGENT_PROTOS=(
    "${SHARED_GRPC_API_CONTRACTS_AGENT_V1_PROTOS[@]}"
    "${SHARED_GRPC_API_CONTRACTS_AGENT_V2_PROTOS[@]}"
    "wendy/agent/apps/v1/recording.proto"
    "wendy/system/v1/notifications.proto"
    "wendylite/v1/sensorlink.proto"
)
SHARED_GRPC_API_CONTRACTS_INPUT="https://github.com/wendylabsinc/service-protos.git#ref=$SHARED_GRPC_API_CONTRACTS_REF"

SHARED_GRPC_API_CONTRACTS_RELAY_PROTOS=(
    "wendycloud/tunnel/v2/tunnel.proto"
    "wendycloud/tunnel/lite/v1/relay.proto"
)

# Exact local sources deferred by the approved migration scope. Each must
# still match a catalogue source; remove its exception with its source migration.
SHARED_GRPC_API_CONTRACTS_LOCAL_EXCEPTIONS=(
    # Retain the deployed OTLP common dependency closure; remove under WDY-4154.
    "opentelemetry/proto/common/v1/common.proto"
    # Retain the deployed OTLP resource dependency closure; remove under WDY-4154.
    "opentelemetry/proto/resource/v1/resource.proto"
    # Retain the deployed OTLP logs dependency closure; remove under WDY-4154.
    "opentelemetry/proto/logs/v1/logs.proto"
    # Retain the deployed OTLP logs collector with its dependencies; remove under WDY-4154.
    "opentelemetry/proto/collector/logs/v1/logs_service.proto"
    # Deployed histogram scale/offset use sint32 rather than int32; remove under WDY-4154.
    "opentelemetry/proto/metrics/v1/metrics.proto"
    # Retain the metrics collector with the incompatible deployed schema; remove under WDY-4154.
    "opentelemetry/proto/collector/metrics/v1/metrics_service.proto"
    # Deployed trace schema differs from the shared catalogue; remove under WDY-4154.
    "opentelemetry/proto/trace/v1/trace.proto"
    # Retain the trace collector with the deployed trace schema; remove under WDY-4154.
    "opentelemetry/proto/collector/trace/v1/trace_service.proto"
    # Local UUID/SignedRequest API conflicts with shared int32/bare requests; remove under WDY-4155.
    "org_invites.proto"
    # Older local Cloud v1 asset fields/RPCs differ; remove under WDY-4157.
    "cloud/assets.proto"
    # Older local Cloud v1 deployment fields/RPCs differ; remove under WDY-4157.
    "cloud/deployments.proto"
    # Local Cloud v1 notification imports/contract differ; remove under WDY-4157.
    "cloud/notifications.proto"
    # Older local Cloud v1 organization API differs; remove under WDY-4157.
    "cloud/organizations.proto"
)

check_shared_grpc_api_contracts_sources() {
    local proto_dir="$1" catalogue_dir="$2" catalogue file proto exception alias
    local seen="" allowed catalogue_proto
    if [ ! -d "$proto_dir" ] || [ ! -d "$catalogue_dir" ]; then
        echo "error: missing local proto tree or shared catalogue export" >&2
        return 1
    fi
    catalogue="$(find "$catalogue_dir" -type f -name '*.proto' -print)" || return $?
    if [ -z "$catalogue" ]; then
        echo "error: shared catalogue export contains no proto files" >&2
        return 1
    fi
    # Parse the complete catalogue too, including APIs not selected to generate.
    buf build "$catalogue_dir" --exclude-source-info -o "$catalogue_dir.binpb" || return $?
    for exception in "${SHARED_GRPC_API_CONTRACTS_LOCAL_EXCEPTIONS[@]}"; do
        # Literal file paths only. A glob, traversal, duplicate or stale entry
        # must not silently broaden the permitted local source tree.
        case "$exception" in
            /*|*..*|*[!a-zA-Z0-9_./-]*|"")
                echo "error: invalid shared source exception: $exception" >&2; return 1 ;;
        esac
        case " $seen " in
            *" $exception "*) echo "error: duplicate shared source exception: $exception" >&2; return 1 ;;
        esac
        catalogue_proto="$exception"
        if [ ! -f "$catalogue_dir/$catalogue_proto" ]; then
            case "$exception" in
                cloud/*)
                    case "${exception#cloud/}" in
                        */*) ;;
                        *) catalogue_proto="${exception#cloud/}" ;;
                    esac ;;
            esac
        fi
        if [ ! -f "$catalogue_dir/$catalogue_proto" ] || [ ! -f "$proto_dir/$exception" ] || [ -L "$proto_dir/$exception" ]; then
            echo "error: shared source exception must name an existing catalogue and regular local file: $exception" >&2
            return 1
        fi
        seen="$seen $exception"
    done
    while IFS= read -r file; do
        proto="${file#"$catalogue_dir/"}"
        alias=""
        # Root catalogue files are staged under cloud/ by legacy consumers.
        case "$proto" in
            wendylite/v1/sensorlink.proto) alias="wendy/lite/sensorlink.proto" ;;
            */*) ;;
            *) alias="cloud/$proto" ;;
        esac
        for file in "$proto" ${alias:+"$alias"}; do
            # An exception covers this local path, never its other spelling.
            allowed=0
            for exception in "${SHARED_GRPC_API_CONTRACTS_LOCAL_EXCEPTIONS[@]}"; do
                if [ "$file" = "$exception" ]; then allowed=1; break; fi
            done
            if [ "$allowed" -eq 1 ]; then continue; fi
            if [ -e "$proto_dir/$file" ] || [ -L "$proto_dir/$file" ]; then
                echo "error: remove Proto/$file; its source is in pinned service-protos" >&2
                return 1
            fi
        done
    done <<< "$catalogue"
}

finish_shared_grpc_api_contracts() {
    local status="$1" scratch="$2"
    if [ "$status" -eq 0 ]; then
        rm -rf "$scratch"
    else
        echo "error: generation failed; temporary inputs and artifacts retained at $scratch" >&2
    fi
}
