# TensorRT Engine Build Process

This document explains how TensorRT engines are built for DeepStream and how to automate pre-building them to speed up container startup.

## Table of Contents

1. [Understanding TensorRT Engines](#understanding-tensorrt-engines)
2. [Current Process: Runtime Build](#current-process-runtime-build)
3. [Automated Process: Pre-built Engines](#automated-process-pre-built-engines)
4. [Extraction Script](#extraction-script)
5. [Multi-Stage Dockerfile Approach](#multi-stage-dockerfile-approach)
6. [Troubleshooting](#troubleshooting)

---

## Understanding TensorRT Engines

### What are TensorRT Engines?

TensorRT engines are **optimized inference runtime files** built from ONNX models. They are:

- **Hardware-specific**: Built for a specific GPU architecture (e.g., Jetson Orin, Xavier)
- **Precision-specific**: FP32, FP16, or INT8
- **Configuration-specific**: Batch size, input dimensions must match

### Engine Naming Convention

DeepStream generates engines with this naming pattern:

```
{onnx_filename}_b{batch_size}_gpu{gpu_id}_{precision}.engine
```

**Example:**
```
Input:  yolo11n.onnx
Output: yolo11n.onnx_b8_gpu0_fp16.engine
```

**Parameters:**
- `yolo11n.onnx` - Original ONNX filename
- `b8` - Batch size 8
- `gpu0` - GPU ID 0
- `fp16` - Half precision (FP16)

### Engine Build Time

| Platform | Build Time | Notes |
|----------|------------|-------|
| Jetson Orin | 5-10 min | First run only |
| Jetson Xavier | 10-15 min | Slower GPU |
| Desktop RTX | 2-5 min | Faster build |

**Why so long?**
TensorRT analyzes the model graph, applies optimizations (layer fusion, kernel auto-tuning), and generates device-specific CUDA kernels. This happens **only once** - subsequent runs load the pre-built engine in seconds.

---

## Current Process: Runtime Build

### How It Works

1. **Container starts** → DeepStream nvinfer plugin checks for engine at configured path
2. **Engine not found** → DeepStream builds it from ONNX model
3. **Engine saved** → Stored at location specified in `model-engine-file`
4. **Future runs** → Pre-built engine loaded instantly

### Configuration

File: `nvinfer_config.txt`

```ini
[property]
# ONNX model to convert
onnx-file=/app/yolo11n.onnx

# Where to save/load the TensorRT engine
model-engine-file=/app/yolo11n.onnx_b8_gpu0_fp16.engine

# Build settings
batch-size=8
network-mode=2        # 0=FP32, 1=INT8, 2=FP16
```

### Pros and Cons

**Pros:**
- ✅ No manual build step required
- ✅ Always matches target hardware
- ✅ Simple deployment

**Cons:**
- ❌ 5-10 minute startup delay on first run
- ❌ Container crashes if build fails
- ❌ No control over build process

---

## Automated Process: Pre-built Engines

### Overview

Pre-build TensorRT engines and include them in the Docker image for instant startup.

### Method 1: Extract from Running Container (Easiest)

#### Step 1: Let Container Build Engine

```bash
# Start detector and wait for engine build to complete
cd detector
wendy run --device wendyos-tender-oar.local
```

Watch logs for completion:
```
INFO: Successfully created engine from ONNX model
INFO: Serialized TensorRT engine to /app/yolo11n.onnx_b8_gpu0_fp16.engine
```

#### Step 2: Extract Engine from Container

Use the provided extraction script `extract_engine.sh`:

```bash
#!/bin/bash
# Extract TensorRT engine from running container

DEVICE="${1:-wendyos-tender-oar.local}"
ENGINE_NAME="${2:-yolo11n.onnx_b8_gpu0_fp16.engine}"
CONTAINER_NAME="detector"

echo "Extracting engine from container on $DEVICE..."

# Get container ID
CONTAINER_ID=$(ssh "root@$DEVICE" "ctr -n default task ls | grep $CONTAINER_NAME | awk '{print \$1}'")

if [ -z "$CONTAINER_ID" ]; then
    echo "Error: Container $CONTAINER_NAME not running on $DEVICE"
    exit 1
fi

# Create temporary directory on host
TEMP_DIR="/tmp/engine_extract_$$"
ssh "root@$DEVICE" "mkdir -p $TEMP_DIR"

# Copy engine from container namespace to host
echo "Copying engine from container..."
ssh "root@$DEVICE" "ctr -n default snapshot export $TEMP_DIR/snapshot.tar $CONTAINER_ID"
ssh "root@$DEVICE" "cd $TEMP_DIR && tar -xf snapshot.tar && cp app/$ENGINE_NAME /tmp/$ENGINE_NAME"

# Download to local machine
echo "Downloading engine to local directory..."
scp "root@$DEVICE:/tmp/$ENGINE_NAME" ./

# Cleanup
ssh "root@$DEVICE" "rm -rf $TEMP_DIR /tmp/$ENGINE_NAME"

echo "✅ Engine saved as ./$ENGINE_NAME"
echo ""
echo "Next steps:"
echo "  1. Add to Dockerfile: COPY $ENGINE_NAME /app/"
echo "  2. Verify nvinfer_config.txt has: model-engine-file=/app/$ENGINE_NAME"
echo "  3. Rebuild: wendy run --device $DEVICE"
```

#### Step 3: Update Dockerfile

Add the pre-built engine to your Dockerfile:

```dockerfile
# Copy YOLO ONNX model
COPY yolo11n.onnx /app/

# Copy pre-built TensorRT engine (skip build on startup)
COPY yolo11n.onnx_b8_gpu0_fp16.engine /app/
```

#### Step 4: Rebuild and Test

```bash
wendy run --device wendyos-tender-oar.local
```

You should see:
```
INFO: Loading engine from /app/yolo11n.onnx_b8_gpu0_fp16.engine
INFO: Successfully loaded engine (skipped build)
```

Startup time: **10 seconds** instead of 5-10 minutes! 🚀

---

### Method 2: Build Engine in Container, Then Extract

Create a temporary container just for building the engine:

```bash
#!/bin/bash
# build_engine_only.sh - Build TensorRT engine without running full detector

DEVICE="${1:-wendyos-tender-oar.local}"

echo "Building TensorRT engine on $DEVICE..."

# Create temporary container with engine build script
cat > /tmp/build_engine.py << 'EOF'
#!/usr/bin/env python3
import sys
sys.path.insert(0, '/opt/venv/lib/python3.10/site-packages')

import gi
gi.require_version('Gst', '1.0')
from gi.repository import Gst, GLib
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

Gst.init(None)

# Create a minimal pipeline just to trigger engine build
pipeline_str = """
    videotestsrc num-buffers=1 !
    video/x-raw,width=640,height=640,framerate=1/1 !
    nvvideoconvert !
    video/x-raw(memory:NVMM),format=NV12 !
    nvinfer config-file-path=/app/nvinfer_config.txt !
    fakesink
"""

logger.info("Creating pipeline to build TensorRT engine...")
pipeline = Gst.parse_launch(pipeline_str)

logger.info("Starting pipeline (this will build the engine)...")
pipeline.set_state(Gst.State.PLAYING)

# Wait for EOS or error
bus = pipeline.get_bus()
msg = bus.timed_pop_filtered(
    600 * Gst.SECOND,  # 10 minute timeout
    Gst.MessageType.EOS | Gst.MessageType.ERROR
)

if msg.type == Gst.MessageType.ERROR:
    err, debug = msg.parse_error()
    logger.error(f"Error: {err.message}")
    sys.exit(1)
elif msg.type == Gst.MessageType.EOS:
    logger.info("✅ Engine built successfully!")
else:
    logger.error("Timeout waiting for engine build")
    sys.exit(1)

pipeline.set_state(Gst.State.NULL)
logger.info("Engine saved at /app/yolo11n.onnx_b8_gpu0_fp16.engine")
EOF

# Copy build script to device
scp /tmp/build_engine.py "root@$DEVICE:/tmp/"

# Run container with build script
ssh "root@$DEVICE" "
    cd /path/to/detector &&
    wendy run --device localhost --entrypoint /opt/venv/bin/python3 -- /tmp/build_engine.py
"

# Extract the engine
./extract_engine.sh "$DEVICE"
```

---

### Method 3: Multi-Stage Dockerfile with Builder (Advanced)

This approach builds the engine during `docker build` using a builder stage:

```dockerfile
# Stage 1: Engine Builder
FROM ubuntu:24.04 AS engine-builder

# Install minimal dependencies for engine build
RUN apt-get update && apt-get install -y \
    python3.10 \
    python3.10-venv \
    gstreamer1.0-tools \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /builder

# Copy model and config
COPY yolo11n.onnx /builder/
COPY nvinfer_config.txt /builder/
COPY build_engine.py /builder/

# Build the engine (requires GPU at build time)
RUN python3.10 build_engine.py

# Stage 2: Runtime Image
FROM ubuntu:24.04

# ... (rest of Dockerfile as before)

# Copy pre-built engine from builder stage
COPY --from=engine-builder /builder/yolo11n.onnx_b8_gpu0_fp16.engine /app/

# ... (rest of Dockerfile)
```

**Note:** This requires `docker build` to run **on the target device** (Jetson) because engines are hardware-specific.

**Better approach:** Use `wendy build` on the device:

```bash
# On Jetson device
ssh root@wendyos-tender-oar.local
cd /path/to/detector
docker build --gpus all -t detector-with-engine .
```

---

## Extraction Script

Save this as `detector/extract_engine.sh`:

```bash
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
    echo "  ssh root@$DEVICE 'ctr -n default task exec --exec-id logs-$$ $CONTAINER_ID tail -f /var/log/detector.log'"
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
