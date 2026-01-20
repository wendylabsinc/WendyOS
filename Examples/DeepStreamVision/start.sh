#!/bin/bash
# DeepStream Vision - Deploy all services to a Wendy device and start local proxy
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

# Kill any existing local proxy (check by port)
if lsof -ti :8080 > /dev/null 2>&1; then
    echo "Stopping existing local proxy on port 8080..."
    lsof -ti :8080 | xargs kill -9 2>/dev/null || true
    sleep 1
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

(
    cd "$SCRIPT_DIR/dashboard"
    echo "[dashboard] Deploying..."
    wendy run --device "$DEVICE" --detach --restart-unless-stopped 2>&1 | tail -5
    echo "[dashboard] Done"
) &
PID_DASHBOARD=$!

# Wait for all deployments to complete
echo "Waiting for deployments to complete..."
wait $PID_GPU $PID_VLM $PID_DETECTOR $PID_DASHBOARD

# Start local proxy in background
echo ""
echo "[local-proxy] Starting..."
cd "$SCRIPT_DIR"
nohup python3 local_proxy.py "$DEVICE" > /tmp/deepstream-proxy.log 2>&1 &
PROXY_PID=$!
sleep 1

# Verify proxy started
if kill -0 $PROXY_PID 2>/dev/null; then
    echo "[local-proxy] Running (PID: $PROXY_PID)"
else
    echo "[local-proxy] Failed to start. Check /tmp/deepstream-proxy.log"
fi

echo ""
echo "========================================="
echo "  Deployment Complete"
echo "========================================="
echo ""
echo "Services will auto-restart on failure."
echo ""
echo "Access Points (via local proxy):"
echo "  Monitor Dashboard: file://$SCRIPT_DIR/monitor.html"
echo "  Local Proxy:       http://localhost:8080"
echo ""
echo "Direct Device Access:"
echo "  Dashboard:        http://$DEVICE:8080"
echo "  VLM API:          http://$DEVICE:8090"
echo "  Detector Metrics: http://$DEVICE:9090/metrics"
echo "  Detector Stream:  http://$DEVICE:9090/stream"
echo "  GPU Metrics:      http://$DEVICE:9091/metrics"
echo ""
echo "Management:"
echo "  List apps:   wendy device apps list --device $DEVICE"
echo "  Stop app:    wendy device apps stop <name> --device $DEVICE"
echo "  View logs:   ssh root@$DEVICE 'ctr task attach <container>'"
echo "  Proxy logs:  tail -f /tmp/deepstream-proxy.log"
echo "  Stop proxy:  pkill -f 'python3.*local_proxy.py'"
echo ""
echo "Logging (optional):"
echo "  1. Start Grafana:  cd monitoring && ./start-monitoring.sh"
echo "  2. Edit detector/Dockerfile: ENV LOKI_HOST=<your-mac-ip>"
echo "  3. Redeploy:       cd detector && wendy run --device $DEVICE --detach"
echo "  4. View logs:      http://localhost:3000 -> Explore -> Loki"
echo ""
