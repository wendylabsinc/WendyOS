#!/bin/bash
# DeepStream Vision - Deploy all services to a Wendy device
# Idempotent: safe to run multiple times, will rebuild/restart as needed
#
# Usage: ./start.sh [device]
# Example: ./start.sh wendyos-graceful-channel.local

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEVICE="${1:-wendyos-tender-oar.local}"

echo "========================================="
echo "  DeepStream Vision Stack"
echo "  Target: $DEVICE"
echo "========================================="
echo ""

# Check if device is reachable
if ! ping -c 1 -W 2 "$DEVICE" > /dev/null 2>&1; then
    echo "Error: Device $DEVICE is not reachable"
    exit 1
fi

# Deploy all services in parallel with auto-restart
# Order doesn't matter - each service handles dependencies gracefully
# --detach: run in background
# --restart-unless-stopped: auto-restart on failure

echo "Deploying services (this may take a minute if rebuilding)..."
echo ""

# Start all deployments in background
(
    cd "$SCRIPT_DIR/gpu-stats"
    echo "[gpu-stats] Deploying..."
    wendy run --device "$DEVICE" --detach --restart-unless-stopped 2>&1 | tail -5
    echo "[gpu-stats] Done"
) &
PID_GPU=$!

(
    cd "$SCRIPT_DIR/vlm"
    echo "[vlm] Deploying..."
    wendy run --device "$DEVICE" --detach --restart-unless-stopped 2>&1 | tail -5
    echo "[vlm] Done"
) &
PID_VLM=$!

(
    cd "$SCRIPT_DIR/detector"
    echo "[detector] Deploying..."
    wendy run --device "$DEVICE" --detach --restart-unless-stopped 2>&1 | tail -5
    echo "[detector] Done"
) &
PID_DETECTOR=$!

# Wait for all deployments to complete
echo "Waiting for deployments to complete..."
wait $PID_GPU $PID_VLM $PID_DETECTOR

echo ""
echo "========================================="
echo "  Deployment Complete"
echo "========================================="
echo ""
echo "Services will auto-restart on failure."
echo ""
echo "Dashboard:"
echo "  Open monitor.html in your browser and enter: $DEVICE"
echo ""
echo "Device Access:"
echo "  VLM API:          http://$DEVICE:8090"
echo "  Detector Metrics: http://$DEVICE:9090/metrics"
echo "  Detector Stream:  http://$DEVICE:9090/stream"
echo "  GPU Metrics:      http://$DEVICE:9091/metrics"
echo ""
echo "Management:"
echo "  List apps:   wendy device apps list --device $DEVICE"
echo "  Stop app:    wendy device apps stop <name> --device $DEVICE"
echo ""
echo "Logs:"
echo "  All logs:          wendy device logs --device $DEVICE"
echo "  Filter by app:     wendy device logs --app detector --device $DEVICE"
echo ""
