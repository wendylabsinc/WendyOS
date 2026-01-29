# Multi-stage build for DeepStream YOLO detector
# Stage 1: Builder - compile custom YOLO library using DeepStream 7.1 base image
# Stage 2: Runtime - minimal Ubuntu 24.04 with compiled library

# ============================================================================
# Stage 1: Builder - Compile custom YOLO library for DeepStream 7.1
# ============================================================================
FROM nvcr.io/nvidia/deepstream-l4t:7.1-triton-multiarch AS builder

# Install build dependencies (including cmake for onnxsim)
RUN apt-get update && apt-get install -y \
    git \
    build-essential \
    cmake \
    && rm -rf /var/lib/apt/lists/*

# Clone DeepStream-Yolo repository and compile custom library for CUDA 12.6
RUN git clone https://github.com/marcoslucianops/DeepStream-Yolo.git /tmp/DeepStream-Yolo \
    && cd /tmp/DeepStream-Yolo \
    && export CUDA_VER=12.6 \
    && make -C nvdsinfer_custom_impl_Yolo \
    && mkdir -p /compiled \
    && cp nvdsinfer_custom_impl_Yolo/libnvdsinfer_custom_impl_Yolo.so /compiled/

# Install ultralytics and export YOLO11n with static batch size for DeepStream
# The export script adds DeepStreamOutput layer that reformats tensors for the custom parser
# Note: --simplify skipped to avoid long onnxsim build time
# Pin torch to 2.3.x for compatibility with export script
RUN pip3 install --no-cache-dir 'torch<2.4' torchvision ultralytics onnx \
    && cd /tmp/DeepStream-Yolo \
    && python3 -c "from ultralytics import YOLO; YOLO('yolo11n.pt')" \
    && python3 utils/export_yolo11.py -w yolo11n.pt --batch 2 -s 640 \
    && mkdir -p /compiled \
    && cp yolo11n.onnx /compiled/ \
    && cp labels.txt /compiled/

# ============================================================================
# Stage 2: Runtime - Minimal Ubuntu 24.04 with Python environment
# ============================================================================
FROM ubuntu:24.04

# Prevent interactive prompts during build
ENV DEBIAN_FRONTEND=noninteractive

# Install minimal system dependencies:
# - Build tools for compiling Python extensions
# - Python 3.10 and development headers (required for pyds)
# - GObject/GStreamer development libraries (for building PyGObject)
# - GStreamer GI typelibs for introspection (runtime introspection)
# - GStreamer runtime plugins (for RTSP, codecs, etc.)
# - libjsoncpp25 (required by DeepStream nvmultiurisrcbin plugin)
#
# Note: DeepStream libraries and CUDA libraries are mounted from the host via CDI (/etc/cdi/nvidia.yaml):
# - /opt/nvidia/deepstream/deepstream-7.1/lib → DeepStream libraries
# - /usr/lib/gstreamer-1.0/deepstream → DeepStream GStreamer plugins
# - /usr/local/cuda-12.6/lib → CUDA runtime libraries
# Add deadsnakes PPA manually (avoids network issues with add-apt-repository)
RUN apt-get update && apt-get install -y \
    build-essential \
    gnupg \
    ca-certificates \
    && echo "deb https://ppa.launchpadcontent.net/deadsnakes/ppa/ubuntu noble main" > /etc/apt/sources.list.d/deadsnakes.list \
    && apt-key adv --keyserver keyserver.ubuntu.com --recv-keys F23C5A6CF475977595C89F51BA6932366A755776 \
    && apt-get update && apt-get install -y \
    python3.10 \
    python3.10-dev \
    python3.10-venv \
    libgirepository1.0-dev \
    libcairo2-dev \
    pkg-config \
    gir1.2-gstreamer-1.0 \
    gir1.2-gst-plugins-base-1.0 \
    gstreamer1.0-tools \
    gstreamer1.0-plugins-base \
    gstreamer1.0-plugins-good \
    gstreamer1.0-plugins-bad \
    gstreamer1.0-libav \
    libjsoncpp25 \
    && rm -rf /var/lib/apt/lists/*

# Create Python 3.10 virtual environment WITHOUT system packages
# We'll install PyGObject directly in the venv to avoid version mismatches
RUN python3.10 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Upgrade pip in venv
RUN pip install --no-cache-dir --upgrade pip setuptools wheel

# Install PyGObject 3.46.0 and pycairo in venv (required for GStreamer bindings)
# Using 3.46.0 specifically as it's the last version that works well with girepository-1.0
RUN pip install --no-cache-dir \
    pycairo>=1.20.0 \
    PyGObject==3.46.0

# Install Python packages in venv
RUN pip install --no-cache-dir \
    flask>=2.0.0 \
    prometheus-client>=0.16.0 \
    'numpy>=1.21.0,<2.0' \
    Pillow>=9.0.0 \
    requests>=2.28.0 \
    opencv-python-headless>=4.5.0 \
    opentelemetry-api \
    opentelemetry-sdk \
    opentelemetry-exporter-otlp-proto-http

# Install DeepStream Python bindings (pyds)
RUN pip install --no-cache-dir \
    https://github.com/NVIDIA-AI-IOT/deepstream_python_apps/releases/download/v1.2.0/pyds-1.2.0-cp310-cp310-linux_aarch64.whl

# Set up working directory
WORKDIR /app

# Create directory for TensorRT engines (if building engines at runtime)
RUN mkdir -p /app/engines

# Create directory for custom YOLO library
RUN mkdir -p /app/lib

# Note: DeepStream plugin directories are created and populated automatically
# by CDI when the container starts. No need to pre-create them.

# Copy compiled custom YOLO library from builder stage (DeepStream 7.1 compatible)
COPY --from=builder /compiled/libnvdsinfer_custom_impl_Yolo.so /app/lib/

# Copy YOLO ONNX model exported with DeepStream-Yolo format (static batch=2)
# This was exported using export_yolo11.py with --batch 2 flag
COPY --from=builder /compiled/yolo11n.onnx /app/

# Copy labels from the export (auto-generated by export script)
COPY --from=builder /compiled/labels.txt /app/labels_exported.txt

# Pre-built TensorRT engine for fast startup (skip 8+ minute build time)
# Engine was built using NvDsInferYoloCudaEngineGet from the custom YOLO library
# on Jetson Orin with DeepStream 7.1 + CUDA 12.6 + TensorRT 10.x
COPY model_b2_gpu0_fp16.engine /app/

# Copy application files
COPY detector.py /app/
COPY vlm_client.py /app/
COPY nvinfer_config.txt /app/
COPY tracker_config.yml /app/
COPY streams.json /app/
COPY labels.txt /app/
COPY entrypoint.sh /app/

# Make scripts executable
RUN chmod +x /app/detector.py /app/entrypoint.sh

# Set environment variables
# Force rebuild: 2026-01-16-v2
ENV PYTHONUNBUFFERED=1
ENV GST_DEBUG=1
# EGL configuration for headless GPU operation (no display server required)
ENV EGL_PLATFORM=device

# DeepStream library and plugin paths (mounted from host via CDI)
# CDI automatically mounts these paths from /etc/cdi/nvidia.yaml:
# - /opt/nvidia/deepstream/deepstream-7.1/lib → DeepStream core libraries
# - /usr/lib/gstreamer-1.0/deepstream → DeepStream GStreamer plugins
# - /usr/local/cuda-12.6/lib → CUDA runtime libraries
# CDI also sets GST_PLUGIN_PATH automatically, but we set it explicitly here
# to ensure plugins can be discovered even if CDI config changes
# VPI library is mounted to /usr/lib via CDI for NvDCF tracker
ENV LD_LIBRARY_PATH=/opt/nvidia/deepstream/deepstream-7.1/lib:/usr/lib/gstreamer-1.0/deepstream:/usr/local/cuda-12.6/lib:/usr/lib/aarch64-linux-gnu:/usr/lib
ENV GST_PLUGIN_PATH=/usr/lib/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream

# Expose Prometheus metrics port
EXPOSE 9090

# Run detector via entrypoint script that sets environment variables
CMD ["/app/entrypoint.sh"]
