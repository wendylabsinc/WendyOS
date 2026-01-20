# DeepStream Vision System - Operations Runbook

## Quick Reference

| Service | Port | Health Check | Logs Command |
|---------|------|--------------|--------------|
| Detector | 9090 | `curl http://<device>:9090/health` | `ssh root@<device> 'ctr -n default task logs detector'` |
| VLM (optional) | 8090 | `curl http://<device>:8090/health` | `ssh root@<device> 'ctr -n default task logs vlm'` |

**Device Address:** `wendyos-tender-oar.local`

---

## Table of Contents

1. [Starting Services](#starting-services)
2. [Stopping Services](#stopping-services)
3. [Viewing Logs](#viewing-logs)
4. [Health Checks](#health-checks)
5. [Accessing Metrics](#accessing-metrics)
6. [Restarting Services](#restarting-services)
7. [Configuration Changes](#configuration-changes)
8. [Troubleshooting](#troubleshooting)
9. [Emergency Procedures](#emergency-procedures)
10. [Maintenance Tasks](#maintenance-tasks)

---

## Starting Services

### Start Detector Service

**Location:** `Examples/DeepStreamVision/detector/`

```bash
# From your laptop
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Expected Output:**
```
ℹ︎ Preparing builder
   ✔︎ Builder ready [0.2s]
ℹ︎ Building and uploading container
   ✔︎ Container built and uploaded successfully! [1.7s]
ℹ︎ Preparing app
   ✔︎ App ready to start [0.3s]
✔ Success
  Started app
```

**Startup Time:**
- With pre-built engine: ~10-15 seconds
- Without pre-built engine: ~15-20 minutes (first time only)

**Logs to Watch For:**
```
INFO: deserialized trt engine from :/app/yolo11n.onnx_b8_gpu0_fp16.engine
INFO: Load new model:/app/nvinfer_config.txt sucessfully
INFO: Starting pipeline...
INFO: Metrics available at http://0.0.0.0:9090/metrics
```

### Start VLM Service (Optional)

**Location:** `Examples/DeepStreamVision/vlm/`

```bash
# From your laptop
cd Examples/DeepStreamVision/vlm
wendy run --device wendyos-tender-oar.local
```

**Startup Time:** ~30-60 seconds (model loading)

**Expected Logs:**
```
INFO: Loading openbmb/MiniCPM-V-2_6...
INFO: INT4 quantization: True
INFO: ✅ Model loaded successfully in 33.2s
INFO: GPU Memory: 2.1GB allocated, 2.3GB reserved
INFO: Starting Flask server on port 8090...
```

### Start Both Services (Recommended Order)

```bash
# Terminal 1: Start VLM first (takes longer to load)
cd Examples/DeepStreamVision/vlm
wendy run --device wendyos-tender-oar.local

# Wait for "Model loaded successfully" message

# Terminal 2: Start Detector
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Why this order?** Detector will detect VLM availability and enable descriptions automatically.

---

## Stopping Services

### Stop Detector Service

**Method 1: From wendy run terminal**
```bash
# Press Ctrl+C in the terminal running 'wendy run'
^C
```

**Method 2: SSH to device**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task kill detector'
```

**Method 3: Force kill (if frozen)**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task kill --signal SIGKILL detector'
```

### Stop VLM Service

```bash
# Method 1: Ctrl+C in wendy run terminal
^C

# Method 2: SSH
ssh root@wendyos-tender-oar.local 'ctr -n default task kill vlm'

# Method 3: Force kill
ssh root@wendyos-tender-oar.local 'ctr -n default task kill --signal SIGKILL vlm'
```

### Stop All Services

```bash
# Kill all running containers
ssh root@wendyos-tender-oar.local 'ctr -n default task ls | grep RUNNING | awk "{print \$1}" | xargs -I {} ctr -n default task kill {}'
```

### Verify Services Stopped

```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task ls'
```

**Expected Output:**
```
TASK        PID    STATUS
detector    1234   STOPPED
vlm 5678   STOPPED
```

---

## Viewing Logs

### Detector Logs

**Live tail (follow logs):**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | tail -f
```

**Last 50 lines:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' | tail -50
```

**Filter for errors:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' | grep -i "error\|warn\|failed"
```

**Filter for performance metrics:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' | grep -i "fps\|latency\|inference"
```

**Check TensorRT engine loading:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' | grep -i "engine\|deserialize\|tensorrt"
```

**Expected logs:**
```
2025-12-18 08:46:46 - __main__ - INFO - Starting DeepStream detector with 1 streams
2025-12-18 08:46:46 - __main__ - INFO - ✅ VLM service connected
2025-12-18 08:46:46 - nvinfer - INFO - deserialized trt engine from :/app/yolo11n.onnx_b8_gpu0_fp16.engine
2025-12-18 08:46:46 - __main__ - INFO - Starting pipeline...
```

### VLM Logs

**Live tail:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm' | tail -f
```

**Last 50 lines:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm 2>&1' | tail -50
```

**Check model loading:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm 2>&1' | grep -i "model\|loaded\|gpu memory"
```

**Expected logs:**
```
2025-12-18 08:30:12 - __main__ - INFO - Loading openbmb/MiniCPM-V-2_6...
2025-12-18 08:30:45 - __main__ - INFO - ✅ Model loaded successfully in 33.2s
2025-12-18 08:30:45 - __main__ - INFO - GPU Memory: 2.1GB allocated
```

### Save Logs to File

**Detector:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' > detector_logs_$(date +%Y%m%d_%H%M%S).txt
```

**VLM:**
```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm 2>&1' > vlm_logs_$(date +%Y%m%d_%H%M%S).txt
```

### View Historical Logs (from local file)

If you used `wendy run` with log redirect:
```bash
# Check local log files
ls -lh /tmp/detector-*.log

# View
tail -f /tmp/detector-run.log
```

---

## Health Checks

### Quick Health Check (All Services)

```bash
# Create a health check script
cat << 'EOF' > check_health.sh
#!/bin/bash
DEVICE="${1:-wendyos-tender-oar.local}"

echo "=== DeepStream Health Check ==="
echo ""

# Detector
echo "Detector Service:"
curl -s http://$DEVICE:9090/health | jq 2>/dev/null || echo "  ❌ Not responding"
echo ""

# VLM
echo "VLM Service:"
curl -s http://$DEVICE:8090/health | jq 2>/dev/null || echo "  ⚠️  Not running (optional)"
echo ""

# GPU
echo "GPU Status:"
ssh root@$DEVICE 'nvidia-smi --query-gpu=utilization.gpu,utilization.memory,memory.used,memory.total --format=csv,noheader'
echo ""

# Containers
echo "Running Containers:"
ssh root@$DEVICE 'ctr -n default task ls | grep RUNNING'
EOF

chmod +x check_health.sh
./check_health.sh wendyos-tender-oar.local
```

### Detector Health

```bash
curl http://wendyos-tender-oar.local:9090/health
```

**Expected Response:**
```json
{
  "status": "healthy",
  "timestamp": "2025-12-18T08:46:46.123456Z"
}
```

**HTTP Status Codes:**
- `200` - Service healthy
- `Connection refused` - Service not running
- `Timeout` - Service frozen

### VLM Health

```bash
curl http://wendyos-tender-oar.local:8090/health
```

**Expected Response:**
```json
{
  "status": "healthy",
  "model_loaded": true,
  "timestamp": "2025-12-18T08:30:45.123456Z"
}
```

**Troubleshooting:**
- `model_loaded: false` - Model still loading, wait 30-60s
- `Connection refused` - VLM service not running

### GPU Health

```bash
ssh root@wendyos-tender-oar.local 'nvidia-smi'
```

**Expected Output:**
```
+-----------------------------------------------------------------------------------------+
| NVIDIA-SMI 535.129.03             Driver Version: 535.129.03     CUDA Version: 12.6     |
|-----------------------------------------+------------------------+----------------------+
| GPU  Name                 Persistence-M | Bus-Id          Disp.A | Volatile Uncorr. ECC |
| Fan  Temp   Perf          Pwr:Usage/Cap |           Memory-Usage | GPU-Util  Compute M. |
|=========================================+========================+======================|
|   0  Orin                       On      | 00000000:00:00.0  Off |                    0 |
| N/A   45C    P0              15W /  25W |    4200MiB /  8192MiB |     68%      Default |
+-----------------------------------------+------------------------+----------------------+
```

**Key Metrics:**
- Temperature: Should be < 60°C (throttles at 80°C)
- Memory: ~1.8GB (detector) + ~2.1GB (VLM) = ~4GB used
- GPU Util: 60-80% normal during inference

### Container Status

```bash
ssh root@wendyos-tender-oar.local 'ctr -n default task ls'
```

**Expected Output:**
```
TASK          PID      STATUS
detector      12345    RUNNING
vlm   12346    RUNNING
```

**Status Meanings:**
- `RUNNING` - Service active and processing
- `STOPPED` - Service stopped cleanly
- `UNKNOWN` - Service in unknown state (usually crashed)

---

## Accessing Metrics

### Prometheus Metrics (Detector)

**View in browser:**
```
http://wendyos-tender-oar.local:9090/metrics
```

**Fetch via curl:**
```bash
curl http://wendyos-tender-oar.local:9090/metrics
```

**Filter specific metrics:**
```bash
# FPS
curl -s http://wendyos-tender-oar.local:9090/metrics | grep "deepstream_fps"

# Inference latency
curl -s http://wendyos-tender-oar.local:9090/metrics | grep "deepstream_inference_latency"

# Detection counts
curl -s http://wendyos-tender-oar.local:9090/metrics | grep "deepstream_detections_total"

# GPU memory
curl -s http://wendyos-tender-oar.local:9090/metrics | grep "deepstream_gpu_memory"
```

### VLM Stats

```bash
curl http://wendyos-tender-oar.local:8090/stats
```

**Expected Response:**
```json
{
  "memory_allocated_gb": 2.1,
  "memory_reserved_gb": 2.3,
  "model_name": "MiniCPM-V-2.6",
  "quantization": "INT4"
}
```

### Create Metrics Dashboard

**Simple monitoring script:**
```bash
cat << 'EOF' > watch_metrics.sh
#!/bin/bash
DEVICE="${1:-wendyos-tender-oar.local}"

while true; do
  clear
  echo "=== DeepStream Metrics Dashboard ==="
  echo "Device: $DEVICE"
  echo "Time: $(date)"
  echo ""

  # FPS
  echo "FPS:"
  curl -s http://$DEVICE:9090/metrics | grep "deepstream_fps{" | sed 's/deepstream_fps/  /'
  echo ""

  # Latency
  echo "Inference Latency (ms):"
  curl -s http://$DEVICE:9090/metrics | grep "deepstream_inference_latency_ms_sum\|deepstream_inference_latency_ms_count" | head -2 | sed 's/deepstream_inference_latency_ms/  /'
  echo ""

  # Detections
  echo "Total Detections:"
  curl -s http://$DEVICE:9090/metrics | grep "deepstream_detections_total" | sed 's/deepstream_detections_total/  /' | head -5
  echo ""

  # GPU
  echo "GPU:"
  ssh root@$DEVICE 'nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader,nounits' | awk '{print "  GPU Util: "$1"%   Memory: "$2"MB"}'
  echo ""

  sleep 2
done
EOF

chmod +x watch_metrics.sh
./watch_metrics.sh wendyos-tender-oar.local
```

### Grafana Dashboard (Optional)

If you have Grafana set up:

**Prometheus Configuration:**
```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'deepstream'
    static_configs:
      - targets: ['wendyos-tender-oar.local:9090']
```

**Useful PromQL Queries:**
```promql
# FPS per stream
deepstream_fps

# Average inference latency
rate(deepstream_inference_latency_ms_sum[1m]) / rate(deepstream_inference_latency_ms_count[1m])

# Detections per second
rate(deepstream_detections_total[1m])

# GPU memory usage
deepstream_gpu_memory_mb
```

---

## Restarting Services

### Restart Detector Only

```bash
# Stop
ssh root@wendyos-tender-oar.local 'ctr -n default task kill detector'

# Wait for stop
sleep 2

# Start
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

### Restart VLM Only

```bash
# Stop
ssh root@wendyos-tender-oar.local 'ctr -n default task kill vlm'

# Wait for stop
sleep 2

# Start
cd Examples/DeepStreamVision/vlm
wendy run --device wendyos-tender-oar.local
```

### Restart After Code Changes

**Detector code changes:**
```bash
# Stop running container
ssh root@wendyos-tender-oar.local 'ctr -n default task kill detector'

# Rebuild and restart
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Note:** Docker build cache means rebuilds are fast (~2-3 seconds if only Python code changed).

### Restart After Configuration Changes

**Stream configuration (streams.json):**
```bash
# Edit streams.json locally
vim Examples/DeepStreamVision/detector/streams.json

# Restart (rebuilds container with new config)
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Inference configuration (nvinfer_config.txt):**
```bash
# Edit config
vim Examples/DeepStreamVision/detector/nvinfer_config.txt

# If you changed batch size or precision, you may need to rebuild engine
# Delete old engine
rm Examples/DeepStreamVision/detector/*.engine

# Restart (will rebuild engine, takes 15 min)
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

### Full System Restart

```bash
# Stop all services
ssh root@wendyos-tender-oar.local 'ctr -n default task ls | grep RUNNING | awk "{print \$1}" | xargs -I {} ctr -n default task kill {}'

# Wait for stop
sleep 5

# Start VLM (optional)
cd Examples/DeepStreamVision/vlm
wendy run --device wendyos-tender-oar.local &

# Wait for VLM to load
sleep 60

# Start Detector
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

---

## Configuration Changes

### Change Video Streams

**Edit:** `Examples/DeepStreamVision/detector/streams.json`

```json
{
  "streams": [
    {
      "name": "camera_1",
      "url": "rtsp://192.168.1.100:8554/stream",
      "enabled": true
    },
    {
      "name": "camera_2",
      "url": "rtsp://192.168.1.101:8554/stream",
      "enabled": true
    }
  ]
}
```

**Apply changes:**
```bash
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Validation:**
- Check logs for "Added stream: camera_1" messages
- Verify FPS metrics appear for each stream
- Check no RTSP connection errors

### Change Detection Model

**Replace YOLO model:**
```bash
# Download new ONNX model
# Place in detector/ directory
cp /path/to/new_model.onnx Examples/DeepStreamVision/detector/

# Update nvinfer_config.txt
vim Examples/DeepStreamVision/detector/nvinfer_config.txt
# Change: onnx-file=/app/new_model.onnx

# Delete old engine (force rebuild)
rm Examples/DeepStreamVision/detector/*.engine

# Update Dockerfile to copy new model
vim Examples/DeepStreamVision/detector/Dockerfile
# Add: COPY new_model.onnx /app/

# Restart (will build new engine, ~15 min)
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

### Change Batch Size

**Edit:** `Examples/DeepStreamVision/detector/nvinfer_config.txt`

```ini
batch-size=4  # Was 8
```

**Important:** Changing batch size requires rebuilding TensorRT engine.

```bash
# Delete old engine
rm Examples/DeepStreamVision/detector/*.engine

# Restart (rebuilds engine with new batch size)
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

**Note:** Engine filename will change to match batch size:
- `yolo11n.onnx_b8_gpu0_fp16.engine` → `yolo11n.onnx_b4_gpu0_fp16.engine`

### Enable/Disable VLM Integration

**VLM is automatically detected at startup.**

**To disable:**
```bash
# Simply don't start VLM service
# Detector will run without VLM descriptions
```

**To enable:**
```bash
# Start VLM service
cd Examples/DeepStreamVision/vlm
wendy run --device wendyos-tender-oar.local

# Detector will detect it automatically
```

**Change VLM parameters:**

Edit `detector/detector.py` lines 93-96:
```python
self.min_interval = 0.1  # Minimum time between VLM calls (100ms = 10/sec)
self.confidence_threshold = 0.8  # Only describe high-confidence detections
self.interesting_classes = {0, 2, 3, 5, 7}  # person, car, motorcycle, bus, truck
```

---

## Troubleshooting

### Problem: Container Won't Start

**Symptoms:**
```
✖ Error
  An unexpected error occurred: Command 'docker buildx build...' failed
```

**Diagnosis:**
```bash
# Check Docker builder
docker buildx ls

# Check device connectivity
ping wendyos-tender-oar.local

# Check SSH access
ssh root@wendyos-tender-oar.local 'echo ok'

# Check disk space on device
ssh root@wendyos-tender-oar.local 'df -h'
```

**Solutions:**

1. **Builder not found:**
   ```bash
   docker buildx create --name wendy-builder --use
   ```

2. **Device unreachable:**
   - Check network connection
   - Verify device is powered on
   - Try IP address instead of hostname

3. **Disk full:**
   ```bash
   # Clean old containers
   ssh root@wendyos-tender-oar.local 'ctr -n default images ls | grep detector | awk "{print \$1}" | xargs -I {} ctr -n default images rm {}'
   ```

### Problem: TensorRT Engine Build Fails

**Symptoms:**
```
ERROR: Failed to build TensorRT engine
ERROR: deserialize engine from file failed
```

**Diagnosis:**
```bash
# Check GPU availability
ssh root@wendyos-tender-oar.local 'nvidia-smi'

# Check CUDA version
ssh root@wendyos-tender-oar.local 'nvcc --version'

# Check free GPU memory
ssh root@wendyos-tender-oar.local 'nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits'
```

**Solutions:**

1. **Insufficient GPU memory:**
   ```bash
   # Stop other GPU processes
   ssh root@wendyos-tender-oar.local 'ctr -n default task ls | grep RUNNING | awk "{print \$1}" | xargs -I {} ctr -n default task kill {}'

   # Restart detector
   cd Examples/DeepStreamVision/detector
   wendy run --device wendyos-tender-oar.local
   ```

2. **Corrupted engine file:**
   ```bash
   # Delete engine and rebuild
   rm Examples/DeepStreamVision/detector/*.engine
   cd Examples/DeepStreamVision/detector
   wendy run --device wendyos-tender-oar.local
   ```

3. **ONNX model issue:**
   ```bash
   # Verify ONNX model
   ls -lh Examples/DeepStreamVision/detector/yolo11n.onnx
   # Should be ~5.4MB

   # Re-download if needed
   # (provide download instructions or URL)
   ```

### Problem: Low FPS / High Latency

**Symptoms:**
```
deepstream_fps{stream="camera_1"} 5.2  # Should be 30+
deepstream_inference_latency_ms 150    # Should be <20
```

**Diagnosis:**
```bash
# Check GPU utilization
ssh root@wendyos-tender-oar.local 'nvidia-smi'

# Check GPU frequency (may be throttled)
ssh root@wendyos-tender-oar.local 'cat /sys/devices/gpu.0/devfreq/17000000.ga10b/cur_freq'

# Check temperature (throttles at 80°C)
ssh root@wendyos-tender-oar.local 'nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader'

# Check CPU usage
ssh root@wendyos-tender-oar.local 'top -bn1 | grep Cpu'
```

**Solutions:**

1. **GPU throttling (overheating):**
   ```bash
   # Check temperature
   ssh root@wendyos-tender-oar.local 'nvidia-smi --query-gpu=temperature.gpu --format=csv,noheader'

   # If > 70°C:
   # - Ensure good airflow
   # - Add heatsink/fan
   # - Reduce workload
   ```

2. **Too many streams:**
   ```bash
   # Reduce number of streams in streams.json
   # Or reduce batch size in nvinfer_config.txt
   ```

3. **Wrong engine precision:**
   ```bash
   # Verify FP16 engine is being used
   ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | grep "deserialize.*fp16"

   # If using FP32, rebuild with FP16
   # Edit nvinfer_config.txt: network-mode=2 (FP16)
   ```

4. **VLM overload:**
   ```bash
   # Check VLM processing time
   ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | grep "VLM"

   # If too many VLM calls, increase rate limit
   # Edit detector.py: self.min_interval = 0.2  # 5/sec instead of 10/sec
   ```

### Problem: RTSP Stream Connection Failed

**Symptoms:**
```
ERROR: Could not connect to 192.168.1.100: Socket I/O timed out
ERROR: Failed to connect. (Timeout while waiting for server response)
```

**Diagnosis:**
```bash
# Test RTSP stream from device
ssh root@wendyos-tender-oar.local 'gst-launch-1.0 rtspsrc location=rtsp://192.168.1.100:8554/stream ! fakesink'

# Check network connectivity from device
ssh root@wendyos-tender-oar.local 'ping 192.168.1.100'

# Check if RTSP port is open
ssh root@wendyos-tender-oar.local 'nc -zv 192.168.1.100 8554'
```

**Solutions:**

1. **Camera/stream not reachable:**
   - Verify camera is on same network as Jetson
   - Check firewall rules
   - Try stream URL in VLC to verify it works

2. **RTSP credentials:**
   ```json
   // Add username/password to URL
   {
     "url": "rtsp://username:password@192.168.1.100:8554/stream"
   }
   ```

3. **Use test stream:**
   ```json
   {
     "url": "rtsp://wowzaec2demo.streamlock.net/vod/mp4:BigBuckBunny_115k.mp4"
   }
   ```

### Problem: No Detections

**Symptoms:**
```
deepstream_detections_total{} 0  # No detections
# Logs show pipeline running but no objects detected
```

**Diagnosis:**
```bash
# Check inference is running
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | grep "inference\|nvinfer"

# Check video is being decoded
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | grep "fps\|frame"

# Check confidence threshold
cat Examples/DeepStreamVision/detector/nvinfer_config.txt | grep "threshold"
```

**Solutions:**

1. **Confidence threshold too high:**
   ```ini
   # nvinfer_config.txt
   pre-cluster-threshold=0.25  # Lower to 0.1 to see more detections
   ```

2. **Wrong input dimensions:**
   ```ini
   # Verify matches model
   infer-dims=3;640;640  # Should be 3;640;640 for YOLO11n
   ```

3. **Model not loading:**
   ```bash
   # Check engine loaded successfully
   ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | grep "Load new model"

   # Should see:
   # INFO: Load new model:/app/nvinfer_config.txt sucessfully
   ```

### Problem: VLM Service Not Available

**Symptoms:**
```
WARNING: VLM service not available
WARNING: VLM service responding but model not loaded yet
```

**Diagnosis:**
```bash
# Check VLM container status
ssh root@wendyos-tender-oar.local 'ctr -n default task ls | grep vlm'

# Check VLM health
curl http://wendyos-tender-oar.local:8090/health

# Check VLM logs
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm' | tail -20
```

**Solutions:**

1. **Model still loading:**
   ```bash
   # Wait 30-60 seconds for model to load
   # Check logs for "Model loaded successfully"
   ```

2. **Insufficient GPU memory:**
   ```bash
   # Check GPU memory
   ssh root@wendyos-tender-oar.local 'nvidia-smi'

   # If memory full, VLM won't load
   # Stop detector to free memory, then start VLM first
   ```

3. **Model download failed:**
   ```bash
   # Check VLM logs for download errors
   ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm' | grep -i "download\|error"

   # May need to restart to retry download
   ```

### Problem: CDI Spec Issues (Missing DeepStream)

**Symptoms:**
```
ERROR: libnvds_meta.so: cannot open shared object file
ERROR: No such element or plugin 'nvinfer'
```

**This means CDI spec is missing DeepStream components.**

**Solution:** Follow the fix documented in `CDI_FIX_APPLIED.md`:

```bash
# 1. Copy fix scripts to device
scp generate_deepstream_csv.sh root@wendyos-tender-oar.local:/tmp/
scp merge_csv.sh root@wendyos-tender-oar.local:/tmp/

# 2. Run on device
ssh root@wendyos-tender-oar.local 'cd /tmp && chmod +x generate_deepstream_csv.sh merge_csv.sh && ./generate_deepstream_csv.sh'

# 3. Merge CSV files
ssh root@wendyos-tender-oar.local 'cd /tmp && ./merge_csv.sh'

# 4. Regenerate CDI spec
ssh root@wendyos-tender-oar.local 'nvidia-ctk cdi generate --csv.file /etc/nvidia-container-runtime/host-files-for-container.d/devices.csv --csv.file /etc/nvidia-container-runtime/host-files-for-container.d/drivers.csv --csv.file /etc/nvidia-container-runtime/host-files-for-container.d/l4t.csv --csv.file /etc/nvidia-container-runtime/host-files-for-container.d/l4t-deepstream.csv --output=/etc/cdi/nvidia.yaml'

# 5. Restart container
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

---

## Emergency Procedures

### System Completely Frozen

```bash
# 1. Hard reset device
ssh root@wendyos-tender-oar.local 'reboot'

# 2. Wait 60 seconds for reboot

# 3. Verify device is back
ping wendyos-tender-oar.local

# 4. Check containers auto-started
ssh root@wendyos-tender-oar.local 'ctr -n default task ls'

# 5. Manually restart if needed
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

### GPU Hung

```bash
# 1. Check GPU status
ssh root@wendyos-tender-oar.local 'nvidia-smi'

# 2. If hung (no response), kill all GPU processes
ssh root@wendyos-tender-oar.local 'killall -9 python3'

# 3. If still hung, reboot
ssh root@wendyos-tender-oar.local 'reboot'
```

### Disk Full

```bash
# 1. Check disk usage
ssh root@wendyos-tender-oar.local 'df -h'

# 2. Clean old container images
ssh root@wendyos-tender-oar.local 'ctr -n default images ls'
ssh root@wendyos-tender-oar.local 'ctr -n default images rm <old-image-sha>'

# 3. Clean build cache
ssh root@wendyos-tender-oar.local 'docker builder prune -af'

# 4. Clean containerd cache
ssh root@wendyos-tender-oar.local 'ctr -n default content prune'
```

### Lost SSH Access

```bash
# 1. Try IP address instead of hostname
ssh root@<device-ip>

# 2. Power cycle device
# (physical power button or power supply)

# 3. Connect monitor and keyboard to device
# Log in via console

# 4. Check network configuration
ip addr show
```

### Rollback to Previous Version

```bash
# If you have previous image in registry
ssh root@wendyos-tender-oar.local 'ctr -n default images ls | grep detector'

# Pull specific version
ssh root@wendyos-tender-oar.local 'ctr -n default images pull wendyos-tender-oar.local:5000/detector:<previous-tag>'

# Or rebuild from git commit
git checkout <previous-commit>
cd Examples/DeepStreamVision/detector
wendy run --device wendyos-tender-oar.local
```

---

## Maintenance Tasks

### Weekly Checks

```bash
# 1. Check system health
./check_health.sh wendyos-tender-oar.local

# 2. Review logs for errors
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector 2>&1' | grep -i "error\|warn" | tail -20

# 3. Check disk space
ssh root@wendyos-tender-oar.local 'df -h'

# 4. Check GPU health
ssh root@wendyos-tender-oar.local 'nvidia-smi'

# 5. Verify metrics are being collected
curl -s http://wendyos-tender-oar.local:9090/metrics | grep "deepstream_fps"
```

### Monthly Maintenance

```bash
# 1. Update system packages on device
ssh root@wendyos-tender-oar.local 'apt update && apt upgrade -y'

# 2. Clean old images
ssh root@wendyos-tender-oar.local 'ctr -n default images prune'

# 3. Restart services to apply updates
# Follow "Restarting Services" section

# 4. Backup configuration
scp root@wendyos-tender-oar.local:/etc/cdi/nvidia.yaml ./backups/nvidia.yaml.$(date +%Y%m%d)
scp root@wendyos-tender-oar.local:/etc/nvidia-container-runtime/host-files-for-container.d/l4t-deepstream.csv ./backups/
```

### Performance Tuning

**Optimize for throughput (max FPS):**
```ini
# nvinfer_config.txt
batch-size=8       # Max batch size
interval=0         # Process every frame
```

**Optimize for latency (min delay):**
```ini
# nvinfer_config.txt
batch-size=1       # Single frame
interval=0         # Process every frame
```

**Optimize for GPU memory:**
```ini
# nvinfer_config.txt
batch-size=4       # Smaller batch
network-mode=2     # FP16 (uses less memory than FP32)
```

### Backup Critical Files

```bash
# Create backup directory
mkdir -p ~/deepstream-backups/$(date +%Y%m%d)

# Backup local config
cp Examples/DeepStreamVision/detector/streams.json ~/deepstream-backups/$(date +%Y%m%d)/
cp Examples/DeepStreamVision/detector/nvinfer_config.txt ~/deepstream-backups/$(date +%Y%m%d)/

# Backup TensorRT engine (avoid rebuild)
cp Examples/DeepStreamVision/detector/*.engine ~/deepstream-backups/$(date +%Y%m%d)/

# Backup device config
scp root@wendyos-tender-oar.local:/etc/cdi/nvidia.yaml ~/deepstream-backups/$(date +%Y%m%d)/
scp root@wendyos-tender-oar.local:/etc/nvidia-container-runtime/host-files-for-container.d/l4t-deepstream.csv ~/deepstream-backups/$(date +%Y%m%d)/
```

---

## Quick Command Reference

```bash
# Start detector
cd Examples/DeepStreamVision/detector && wendy run --device wendyos-tender-oar.local

# Start VLM
cd Examples/DeepStreamVision/vlm && wendy run --device wendyos-tender-oar.local

# View detector logs
ssh root@wendyos-tender-oar.local 'ctr -n default task logs detector' | tail -f

# View VLM logs
ssh root@wendyos-tender-oar.local 'ctr -n default task logs vlm' | tail -f

# Check health
curl http://wendyos-tender-oar.local:9090/health
curl http://wendyos-tender-oar.local:8090/health

# View metrics
curl http://wendyos-tender-oar.local:9090/metrics

# Stop detector
ssh root@wendyos-tender-oar.local 'ctr -n default task kill detector'

# Stop VLM
ssh root@wendyos-tender-oar.local 'ctr -n default task kill vlm'

# Check GPU
ssh root@wendyos-tender-oar.local 'nvidia-smi'

# Check containers
ssh root@wendyos-tender-oar.local 'ctr -n default task ls'

# Reboot device
ssh root@wendyos-tender-oar.local 'reboot'
```

---

## Support and Documentation

**Project Documentation:**
- `ARCHITECTURE.md` - System architecture overview
- `CDI_CSV_ENVIRONMENT_SETUP.md` - CDI configuration guide
- `TENSORRT_ENGINE_BUILD.md` - TensorRT engine documentation
- `QUICKSTART_TENSORRT_ENGINE.md` - Quick engine setup guide
- `YOCTO_DEPENDENCIES.md` - Yocto build dependencies

**Helpful Commands:**
- `wendy --help` - Wendy CLI help
- `gst-inspect-1.0 nvinfer` - DeepStream plugin details
- `nvidia-smi --help-query-gpu` - GPU monitoring options

**Troubleshooting Resources:**
- Check logs first: Both detector and VLM logs
- Verify health endpoints: `/health` for both services
- Monitor GPU: `nvidia-smi` shows temperature, memory, utilization
- Test streams: Use VLC or `gst-launch-1.0` to test RTSP URLs
