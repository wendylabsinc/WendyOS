#!/bin/bash
# WendyOS Dev Registry Manager
# Helper script for wendy-agent to start/stop the development container registry
# The registry uses containerd's content store (no duplicate storage)

set -e

# Ensure PATH includes standard locations (needed when called by systemd services)
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

NAMESPACE="default"
CONTAINER_NAME="wendyos-dev-registry"
IMAGE_NAME="wendyos/containerd-registry:latest"
LISTEN_ADDRESS="${LISTEN_ADDRESS:-0.0.0.0:5000}"

# Use absolute paths for commands (in case PATH is not set properly)
CTR="/usr/bin/ctr"
GREP="/bin/grep"
CURL="/usr/bin/curl"

usage() {
    cat <<EOF
WendyOS Dev Registry Manager

Usage: $(basename "$0") {start|stop|status|restart|logs} [listen_address]

Commands:
    start [addr]  - Start the dev registry container
    stop          - Stop the dev registry container
    status        - Check if the registry is running
    restart [addr]- Restart the registry
    logs          - Show registry container logs

Arguments:
    listen_address - Optional listen address:port (default: 127.0.0.1:5000)

Environment:
    LISTEN_ADDRESS - Registry listen address (overridden by command-line arg)

Examples:
    # Start registry on default address (0.0.0.0:5000)
    sudo $(basename "$0") start

    # Start registry on custom port
    sudo $(basename "$0") start 0.0.0.0:6000

    # Start on loopback only (more secure)
    sudo $(basename "$0") start 127.0.0.1:5000

    # Access from dev machine via SSH port-forward:
    ssh -L 5000:127.0.0.1:5000 wendyos@device.local

    # Then push/pull images:
    docker tag myimage:latest localhost:5000/myimage:latest
    docker push localhost:5000/myimage:latest
EOF
}

check_image_exists() {
    if ! $CTR -n "${NAMESPACE}" images ls | $GREP -q "${IMAGE_NAME}"; then
        echo "ERROR: Registry image '${IMAGE_NAME}' not found in namespace '${NAMESPACE}'"
        echo "Did the import service run? Check: systemctl status wendyos-dev-registry-import.service"
        echo "Or manually import: sudo $CTR -n ${NAMESPACE} images import /usr/share/wendyos/offline-images/containerd-registry-arm64.tar"
        exit 1
    fi
}

start_registry() {
    echo "Starting WendyOS dev registry..."

    # Check if image exists
    check_image_exists

    # Check if already running
    if $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Registry container is already running"
        return 0
    fi

    # Check if container exists but is stopped
    if $CTR -n "${NAMESPACE}" containers ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Starting existing container..."
        $CTR -n "${NAMESPACE}" tasks start -d "${CONTAINER_NAME}"
    else
        echo "Creating new registry container..."
        # Create container with host networking and containerd socket access
        # Note: Registry needs access to /run/containerd/containerd.sock to read/write images
        $CTR -n "${NAMESPACE}" run \
            --detach \
            --net-host \
            --mount type=bind,src=/run/containerd/containerd.sock,dst=/run/containerd/containerd.sock,options=rbind:rw \
            --env LISTEN_ADDRESS="${LISTEN_ADDRESS}" \
            --env CONTAINERD_NAMESPACE="${NAMESPACE}" \
            --env LOG_FORMAT=json \
            "${IMAGE_NAME}" \
            "${CONTAINER_NAME}"
    fi

    echo "✅ Dev registry started on ${LISTEN_ADDRESS}"
    if [[ "${LISTEN_ADDRESS}" == 127.0.0.1:* ]]; then
        echo "Registry is loopback-only. Access via SSH port-forward: ssh -L 5000:127.0.0.1:5000 wendyos@<device>"
    else
        echo "Registry is accessible on the network at ${LISTEN_ADDRESS}"
    fi
}

stop_registry() {
    echo "Stopping WendyOS dev registry..."

    if ! $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "Registry is not running"
        return 0
    fi

    # Kill the task
    $CTR -n "${NAMESPACE}" tasks kill "${CONTAINER_NAME}" || true
    sleep 1

    # Delete the task
    $CTR -n "${NAMESPACE}" tasks delete "${CONTAINER_NAME}" || true

    echo "✅ Dev registry stopped"
}

status_registry() {
    if $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "✅ Registry is running"
        $CTR -n "${NAMESPACE}" tasks ls | $GREP "${CONTAINER_NAME}"
        return 0
    else
        echo "❌ Registry is not running"
        return 1
    fi
}

logs_registry() {
    if ! $CTR -n "${NAMESPACE}" tasks ls | $GREP -q "${CONTAINER_NAME}"; then
        echo "ERROR: Registry is not running"
        exit 1
    fi

    echo "Showing logs for ${CONTAINER_NAME}..."
    # Note: $CTR doesn't have a native 'logs' command, we need to use tasks metrics or journald
    # For now, just show task info. Logs go to containerd's stdout/stderr
    echo "Container task info:"
    $CTR -n "${NAMESPACE}" tasks ls | $GREP "${CONTAINER_NAME}"
    echo ""
    echo "For full logs, check containerd's journal: journalctl -u containerd | $GREP ${CONTAINER_NAME}"
}

restart_registry() {
    stop_registry
    sleep 1
    start_registry
}

# Main command dispatch
COMMAND="${1:-}"
CUSTOM_LISTEN_ADDRESS="${2:-}"

# Override LISTEN_ADDRESS if provided as argument
if [ -n "$CUSTOM_LISTEN_ADDRESS" ]; then
    LISTEN_ADDRESS="$CUSTOM_LISTEN_ADDRESS"
fi

case "$COMMAND" in
    start)
        start_registry
        ;;
    stop)
        stop_registry
        ;;
    status)
        status_registry
        ;;
    restart)
        restart_registry
        ;;
    logs)
        logs_registry
        ;;
    *)
        usage
        exit 1
        ;;
esac
