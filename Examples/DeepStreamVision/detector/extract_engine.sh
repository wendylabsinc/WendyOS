#!/bin/bash
# Extract TensorRT engine from running detector container
# Usage: ./extract_engine.sh [device] [engine-name]

set -e

DEVICE="${1:-wendyos-tender-oar.local}"
ENGINE_NAME="${2:-yolo11n.onnx_b8_gpu0_fp16.engine}"
CONTAINER_NAME="detector"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "TensorRT Engine Extraction Tool"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Device:     $DEVICE"
echo "Container:  $CONTAINER_NAME"
echo "Engine:     $ENGINE_NAME"
echo ""

# Check if container is running
echo "[1/5] Checking if container is running..."
CONTAINER_ID=$(ssh "root@$DEVICE" "ctr -n default task ls | grep $CONTAINER_NAME | awk '{print \$1}'" 2>/dev/null || echo "")

if [ -z "$CONTAINER_ID" ]; then
    echo "❌ Error: Container '$CONTAINER_NAME' not found on $DEVICE"
    echo ""
    echo "Make sure the container is running:"
    echo "  cd detector && wendy run --device $DEVICE"
    exit 1
fi

echo "✅ Container found: $CONTAINER_ID"

# Check if engine exists in container
echo ""
echo "[2/5] Checking if engine exists in container..."
ENGINE_EXISTS=$(ssh "root@$DEVICE" "ctr -n default task exec --exec-id check-$$ $CONTAINER_ID test -f /app/$ENGINE_NAME && echo yes || echo no" 2>/dev/null || echo "no")

if [ "$ENGINE_EXISTS" != "yes" ]; then
    echo "❌ Error: Engine file /app/$ENGINE_NAME not found in container"
    echo ""
    echo "The engine might still be building. Check container logs:"
    echo "  ssh root@$DEVICE 'ctr -n default task logs $CONTAINER_ID'"
    exit 1
fi

echo "✅ Engine file exists"

# Copy engine from container
echo ""
echo "[3/5] Copying engine from container to host..."
TEMP_DIR="/tmp/engine_extract_$$"
ssh "root@$DEVICE" "mkdir -p $TEMP_DIR"

# Use ctr task exec to copy the file out
ssh "root@$DEVICE" "ctr -n default task exec --exec-id copy-$$ $CONTAINER_ID cat /app/$ENGINE_NAME > $TEMP_DIR/$ENGINE_NAME"

echo "✅ Engine copied to host:/tmp"

# Download engine to local machine
echo ""
echo "[4/5] Downloading engine to local directory..."
scp -q "root@$DEVICE:$TEMP_DIR/$ENGINE_NAME" ./

FILE_SIZE=$(ls -lh "$ENGINE_NAME" | awk '{print $5}')
echo "✅ Downloaded: $ENGINE_NAME ($FILE_SIZE)"

# Cleanup
echo ""
echo "[5/5] Cleaning up temporary files..."
ssh "root@$DEVICE" "rm -rf $TEMP_DIR"
echo "✅ Cleanup complete"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Engine extraction complete!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Next steps to use pre-built engine:"
echo ""
echo "  1. Update Dockerfile to copy the engine:"
echo "     COPY $ENGINE_NAME /app/"
echo ""
echo "  2. Verify nvinfer_config.txt has correct path:"
echo "     model-engine-file=/app/$ENGINE_NAME"
echo ""
echo "  3. Rebuild and run:"
echo "     wendy run --device $DEVICE"
echo ""
echo "  4. Verify fast startup (should be ~10 seconds instead of 5-10 minutes)"
echo ""
