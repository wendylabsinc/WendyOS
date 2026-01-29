# DeepStream Vision

Real-time object detection and scene understanding on Jetson devices using DeepStream and vision language models.

## What This Does

- **YOLO Detection**: Runs YOLO11n at 20+ FPS on Jetson Orin
- **VLM Descriptions**: Optional AI scene descriptions using Qwen3-VL
- **Live Dashboard**: Web UI showing detections, metrics, and video stream
- **Prometheus Metrics**: FPS, latency, detection counts for monitoring

## Prerequisites

1. **Wendy CLI** installed on your Mac
2. **Jetson device** running WendyOS (Orin recommended)
3. **RTSP camera** or video source
4. **Hugging Face account** (for VLM, optional)

## Quick Start

### 1. Connect to Your Device

First, make sure your Jetson is connected to WiFi:

```bash
# Find your device
wendy device list

# Connect to WiFi (if needed)
wendy device wifi connect --device <your-device>.local --ssid "YourWiFi" --password "YourPassword"

# Verify connectivity
ping <your-device>.local
ssh root@<your-device>.local
```

### 2. Configure Your Camera

Edit `detector/streams.json` with your RTSP camera URL:

```json
{
  "streams": [
    {
      "name": "camera1",
      "url": "rtsp://192.168.1.100:554/stream",
      "enabled": true
    }
  ]
}
```

### 3. Deploy Everything

```bash
cd Examples/DeepStreamVision
./start.sh <your-device>.local
```

This deploys all services in parallel:
- **detector** - YOLO object detection (port 9090)
- **gpu-stats** - GPU monitoring (port 9091)
- **vlm** - Vision language model (port 8090, optional)

### 4. View the Dashboard

Open `monitor.html` in your browser and enter your device hostname (e.g. `wendyos-tender-oar.local`):

```bash
open monitor.html
```

The dashboard connects directly to the device services via CORS. You can also access them individually:
- Metrics: `http://<your-device>.local:9090/metrics`
- Stream: `http://<your-device>.local:9090/stream`

## Setting Up VLM (Optional)

The VLM service provides AI-generated descriptions of detected objects. It requires a Hugging Face token.

### Get a Hugging Face Token

1. Create account at https://huggingface.co/join
2. Go to https://huggingface.co/settings/tokens
3. Click "New token" → Name: `deepstream-vlm` → Type: **Read**
4. Copy the token (starts with `hf_...`)

### Configure the Token

The VLM model is already bundled in the container. You just need to set the token if you want to download updates:

```bash
# Option 1: Set in Dockerfile (for permanent use)
# Edit vlm/Dockerfile and add:
ENV HF_TOKEN=hf_your_token_here

# Option 2: Pass at runtime
HF_TOKEN=hf_your_token_here wendy run --device <device>.local --detach
```

**Note:** The Qwen3-VL-2B-Instruct model is cached on the device, so you don't need a token for basic operation.

## Services

| Service | Port | Description |
|---------|------|-------------|
| detector | 9090 | YOLO detection, Prometheus metrics, MJPEG stream |
| vlm | 8090 | Qwen3-VL-2B vision language model (INT4) |
| gpu-stats | 9091 | GPU temperature, memory, utilization |

## Useful Commands

```bash
# Check running apps
wendy device apps list --device <device>.local

# Stop an app
wendy device apps stop detector --device <device>.local

# View logs
wendy device logs --device <device>.local
wendy device logs --app detector --device <device>.local

# Health checks
curl http://<device>.local:9090/health
curl http://<device>.local:8090/health

# View metrics
curl http://<device>.local:9090/metrics | grep deepstream_fps
```

## Troubleshooting

### Device not reachable
```bash
# Check WiFi connection
wendy device wifi status --device <device>.local

# Reconnect WiFi
wendy device wifi connect --device <device>.local --ssid "YourWiFi" --password "YourPassword"
```

### Detector not starting
```bash
# Check container status
ssh root@<device>.local 'ctr -n default task ls'

# View logs
ssh root@<device>.local 'ctr -n default task exec detector cat /proc/1/fd/1' | tail -50
```

### VLM not responding
- VLM takes ~10 minutes to load on first run (model quantization)
- Subsequent starts are faster (~60 seconds)
- Check health: `curl http://<device>.local:8090/health`
- VLM requires ~1.5GB GPU memory (INT4 quantization)

### Low FPS
- Check GPU temperature: `ssh root@<device>.local 'cat /sys/class/thermal/thermal_zone*/temp'`
- Reduce streams or batch size in `detector/nvinfer_config.txt`

## Viewing Logs

All services emit logs via OpenTelemetry to the WendyOS agent. Use the Wendy CLI to tail logs:

```bash
# All logs
wendy device logs --device <device>.local

# Filter by service
wendy device logs --app detector --device <device>.local
wendy device logs --app vlm --device <device>.local
wendy device logs --app gpu-stats --device <device>.local
```

## Project Structure

```
DeepStreamVision/
├── start.sh              # Deploy all services
├── monitor.html          # Local dashboard (connects directly to device)
├── detector/             # YOLO detection service
│   ├── detector.py       # Main detection code
│   ├── streams.json      # Camera configuration
│   └── nvinfer_config.txt # TensorRT inference config
├── vlm/                  # Vision language model
│   ├── qwen3_service.py  # Qwen3-VL API server (INT4 quantized)
│   └── models/           # Model cache directory
└── gpu-stats/            # GPU monitoring
```

## More Documentation

- [vlm/HUGGINGFACE_SETUP.md](vlm/HUGGINGFACE_SETUP.md) - HuggingFace token setup
