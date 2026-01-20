# Qwen2.5-VL Vision-Language Model Service
# Optimized for Jetson with INT4 quantization using Jetson-compatible bitsandbytes
# Model is baked into the image for fast startup (no downloads needed)

FROM ubuntu:22.04

# Install system dependencies and native libraries
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

# Upgrade pip to fix dependency resolution issues in Ubuntu 22.04
RUN pip3 install --no-cache-dir --upgrade pip

# Install PyTorch from NVIDIA's Jetson-optimized wheels FIRST
# These are pre-compiled for ARM64 and optimized for Jetson with CUDA 12.6
# This leverages CDI-mapped CUDA/cuDNN from the host system
RUN pip3 install --no-cache-dir \
    torch==2.8.0 \
    torchvision==0.23.0 \
    torchaudio==2.8.0 \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Install bitsandbytes from Jetson AI Lab (ARM64-compatible build)
RUN pip3 install --no-cache-dir \
    bitsandbytes \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Install ML and service dependencies from PyPI
# Install huggingface-hub first for model downloading (doesn't require CUDA)
RUN pip3 install --no-cache-dir \
    huggingface-hub \
    transformers>=4.36.0 \
    accelerate \
    flask \
    pillow \
    numpy==1.26.4 \
    qwen-vl-utils \
    av

# Copy pre-downloaded Qwen2.5-VL-3B-Instruct model from local machine
# This is kept separate from Docker build cache for faster iteration
# Download once with: hf download Qwen/Qwen2.5-VL-3B-Instruct --local-dir ./models/Qwen2.5-VL-3B-Instruct
# The model is ~6GB and copied directly into the image
COPY models/Qwen2.5-VL-3B-Instruct /app/models/Qwen2.5-VL-3B-Instruct

# Set working directory
WORKDIR /app

# Copy service script
COPY qwen_service.py /app/

# Make executable
RUN chmod +x /app/qwen_service.py

# Set environment variables
ENV PYTHONUNBUFFERED=1

# Expose API port
EXPOSE 8090

CMD ["python3", "/app/qwen_service.py"]
