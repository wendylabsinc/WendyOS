# Qwen3-VL Vision-Language Model Service
# Optimized for Jetson Orin Nano with INT4 quantization using Jetson-compatible bitsandbytes
# Model is baked into the image for fast startup (no downloads needed)

FROM ubuntu:22.04

# Install system dependencies and native libraries for bitsandbytes
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip \
    python3-dev \
    libopenblas-dev \
    libblas-dev \
    liblapack-dev \
    gfortran \
    cmake \
    build-essential \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Copy pre-downloaded Qwen3-VL-2B model EARLY for better layer caching
# Download once with: huggingface-cli download Qwen/Qwen3-VL-2B-Instruct --local-dir ./models/Qwen3-VL-2B-Instruct
COPY models/Qwen3-VL-2B-Instruct /app/models/Qwen3-VL-2B-Instruct

# Upgrade pip
RUN pip3 install --no-cache-dir --upgrade pip

# Install PyTorch from Jetson AI Lab (ARM64-optimized, uses CDI-mapped CUDA)
# Note: PyTorch 2.9.1 requires libcudss.so.0 which isn't available on WendyOS yet
# Staying on 2.8.0 until WendyOS CUDA libraries are updated
RUN pip3 install --no-cache-dir \
    torch==2.8.0 \
    torchvision==0.23.0 \
    torchaudio==2.8.0 \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Install bitsandbytes and torchao from Jetson AI Lab (ARM64-compatible builds)
# These are critical - the PyPI versions do NOT work on Jetson
# Note: flash-attn requires PyTorch 2.9+ which needs libcudss.so.0 (not yet on WendyOS)
RUN pip3 install --no-cache-dir \
    bitsandbytes \
    torchao \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Install ML and service dependencies
# transformers >= 4.57.0 required for Qwen3-VL support
RUN pip3 install --no-cache-dir \
    huggingface-hub \
    "transformers>=4.57.0" \
    accelerate \
    flask \
    pillow \
    "numpy<2" \
    qwen-vl-utils \
    av \
    opentelemetry-api \
    opentelemetry-sdk \
    opentelemetry-exporter-otlp-proto-http

# Set working directory
WORKDIR /app

# Copy service script
COPY qwen3_service.py /app/

# Set environment variables
ENV PYTHONUNBUFFERED=1
ENV PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True

# Expose API port
EXPOSE 8090

CMD ["python3", "/app/qwen3_service.py"]
