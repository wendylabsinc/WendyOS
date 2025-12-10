FROM ubuntu:22.04

# Workaround for containerd overlayfs snapshot issues
RUN mkdir -p /usr/local/bin

# Install system dependencies including OpenBLAS (required by PyTorch)
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip \
    python3-dev \
    build-essential \
    git \
    libopenblas-dev \
    libblas-dev \
    liblapack-dev \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Install PyTorch from NVIDIA's Jetson-optimized wheels
# These are pre-compiled for ARM64 and optimized for Jetson with CUDA 12.6
# This leverages CDI-mapped CUDA/cuDNN from the host system
RUN pip3 install --no-cache-dir \
    torch==2.8.0 \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Pin NumPy to 1.x (Jetson PyTorch was compiled against NumPy 1.x)
RUN pip3 install --no-cache-dir numpy==1.26.4

# Install HuggingFace Transformers and dependencies
RUN pip3 install --no-cache-dir \
    transformers \
    accelerate \
    sentencepiece \
    flask

# Verify we got the correct PyTorch version (will fail build if wrong)
# Use pip show instead of import to avoid CUDA library loading during build
RUN pip3 show torch | grep "Version: 2.8.0" && echo "Correct PyTorch version installed"

# Set working directory and copy application code
WORKDIR /app
COPY app.py .

# NOTE: Cannot use non-root user - CUDA memory manager needs root access to /dev/nvmap
# RUN useradd --create-home --shell /bin/bash app && \
#     chown -R app:app /app
# USER app

# Set environment variables
ENV PYTHONUNBUFFERED=1
ENV HF_HOME=/app/.cache/huggingface
ENV TRANSFORMERS_CACHE=/app/.cache/huggingface

# Default command
CMD ["python3", "app.py"]
